package l2

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	anchoraws "github.com/anchor-dra/anchor/internal/aws"
	"github.com/anchor-dra/anchor/internal/constants"
	"github.com/anchor-dra/anchor/internal/model"
)

func TestValidateOnPremisesEndpoint(t *testing.T) {
	strategy := New(nil, nil)
	endpoint := anchoraws.Endpoint{Path: model.PlacementPath{
		Name: "a", IP: "10.50.1.10/24", SubnetCIDR: "10.50.1.0/24",
		Interface: "carrier0", ParentMAC: "02:00:00:00:00:01", ENIID: "worker-1/carrier0",
	}}
	if err := strategy.Validate(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	endpoint.Path.IP = "10.60.1.10/24"
	if err := strategy.Validate(context.Background(), endpoint); err == nil {
		t.Fatal("expected an address outside the configured carrier subnet to fail")
	}
}

func TestTransferFromUnreadyNodeRequiresExplicitFence(t *testing.T) {
	oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-old"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}}
	strategy := New(fake.NewSimpleClientset(oldNode), nil)
	endpoint := anchoraws.Endpoint{NodeName: "worker-new", PreviousENI: "worker-old/carrier0", Path: model.PlacementPath{
		Name: "a", IP: "10.50.1.10/24", SubnetCIDR: "10.50.1.0/24", Interface: "carrier0",
		ParentMAC: "02:00:00:00:00:01", ENIID: "worker-new/carrier0",
	}}
	if err := strategy.Place(context.Background(), endpoint); err == nil {
		t.Fatal("expected an unfenced transfer away from an unready node to fail")
	}
	oldNode.Annotations = map[string]string{constants.FencedAnnotation: "true"}
	strategy = New(fake.NewSimpleClientset(oldNode), nil)
	if err := strategy.Place(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
}
