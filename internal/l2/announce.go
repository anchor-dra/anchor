package l2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	anchoraws "github.com/anchor-dra/anchor/internal/aws"
	"github.com/anchor-dra/anchor/internal/constants"
)

// Announce is the controller-side half of the on-premises placement strategy.
// Durable address ownership is fenced by the common EndpointOwnership
// reconciler. The node injector performs the actual gratuitous ARP only after
// the interface exists in the selected pod namespace.
type Announce struct {
	Core   kubernetes.Interface
	Logger *slog.Logger
}

func New(core kubernetes.Interface, logger *slog.Logger) *Announce {
	return &Announce{Core: core, Logger: logger}
}

func (s *Announce) Validate(_ context.Context, endpoint anchoraws.Endpoint) error {
	path := endpoint.Path
	if path.Interface == "" || path.ParentMAC == "" || path.ENIID == "" {
		return fmt.Errorf("l2-announce path %q has incomplete approved parent identity", path.Name)
	}
	prefix, err := netip.ParsePrefix(path.IP)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("l2-announce path %q has invalid IPv4 address %q", path.Name, path.IP)
	}
	subnet, err := netip.ParsePrefix(path.SubnetCIDR)
	if err != nil || !subnet.Contains(prefix.Addr()) || prefix.Bits() != subnet.Bits() {
		return fmt.Errorf("l2-announce address %q does not match subnet %q", path.IP, path.SubnetCIDR)
	}
	return nil
}

func (s *Announce) Place(ctx context.Context, endpoint anchoraws.Endpoint) error {
	if err := s.Validate(ctx, endpoint); err != nil {
		return err
	}
	if err := s.validateTransferFence(ctx, endpoint); err != nil {
		return err
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("on-premises endpoint ownership fenced; awaiting namespace injection and L2 announcement",
		"claimUID", endpoint.ClaimUID, "claim", endpoint.ClaimName, "node", endpoint.NodeName,
		"parent", endpoint.Path.Interface, "ip", endpoint.Path.IP)
	return nil
}

func (s *Announce) validateTransferFence(ctx context.Context, endpoint anchoraws.Endpoint) error {
	if endpoint.PreviousENI == "" || endpoint.PreviousENI == endpoint.Path.ENIID {
		return nil
	}
	oldNode, _, found := strings.Cut(endpoint.PreviousENI, "/")
	if !found || oldNode == "" {
		return fmt.Errorf("previous l2 parent identity %q is invalid", endpoint.PreviousENI)
	}
	if oldNode == endpoint.NodeName {
		return nil
	}
	if s.Core == nil {
		return errors.New("Kubernetes client is required to validate an l2 transfer fence")
	}
	node, err := s.Core.CoreV1().Nodes().Get(ctx, oldNode, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("verify previous l2 owner node %s: %w", oldNode, err)
	}
	if strings.EqualFold(node.Annotations[constants.FencedAnnotation], "true") {
		return nil
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			// A move away from a healthy node is serialized by DRA unprepare. A
			// NotReady node needs explicit infrastructure fencing because its old
			// pod namespace may still be running beyond the control-plane view.
			return nil
		}
	}
	return fmt.Errorf("previous l2 owner node %s is not Ready and lacks %s=true fencing evidence", oldNode, constants.FencedAnnotation)
}

func (s *Announce) Release(context.Context, anchoraws.Endpoint) error { return nil }

func (s *Announce) PlaceBatch(ctx context.Context, endpoints []anchoraws.Endpoint) []error {
	results := make([]error, len(endpoints))
	for i := range endpoints {
		results[i] = s.Place(ctx, endpoints[i])
	}
	return results
}

func (s *Announce) VerifyBatch(ctx context.Context, endpoints []anchoraws.Endpoint) ([]anchoraws.VerificationResult, error) {
	results := make([]anchoraws.VerificationResult, len(endpoints))
	for i := range endpoints {
		err := s.Validate(ctx, endpoints[i])
		results[i] = anchoraws.VerificationResult{Placed: err == nil, Err: err}
	}
	return results, nil
}

var _ anchoraws.BatchPlacementStrategy = (*Announce)(nil)
