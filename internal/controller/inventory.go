package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/anchor-dra/anchor/internal/constants"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/model"
)

type DiscoveryAPI interface {
	DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error)
	DescribeSubnets(context.Context, *awsec2.DescribeSubnetsInput, ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error)
}

type InventoryReconciler struct {
	Core      kubernetes.Interface
	Dynamic   dynamic.Interface
	EC2       DiscoveryAPI
	NodeLabel string
	Logger    *slog.Logger
}

func (r *InventoryReconciler) Reconcile(ctx context.Context) error {
	profiles, err := r.profiles(ctx)
	if err != nil {
		return err
	}
	nodes, err := r.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: r.NodeLabel})
	if err != nil {
		return fmt.Errorf("list enabled nodes: %w", err)
	}
	var nodeErrors []error
	for i := range nodes.Items {
		node := &nodes.Items[i]
		instanceID := instanceID(node.Spec.ProviderID)
		if instanceID == "" {
			r.Logger.Warn("enabled node has no AWS provider ID", "node", node.Name)
			continue
		}
		if err := r.reconcileNode(ctx, node.Name, instanceID, node.Labels["topology.kubernetes.io/zone"], profiles); err != nil {
			nodeErrors = append(nodeErrors, fmt.Errorf("reconcile inventory for node %s: %w", node.Name, err))
		}
	}
	return errors.Join(nodeErrors...)
}

func (r *InventoryReconciler) profiles(ctx context.Context) (map[string]model.DeviceClassParameters, error) {
	classes, err := r.Core.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list DeviceClasses: %w", err)
	}
	profiles := map[string]model.DeviceClassParameters{}
	for i := range classes.Items {
		class := &classes.Items[i]
		for _, config := range class.Spec.Config {
			if config.Opaque == nil || config.Opaque.Driver != constants.DriverName {
				continue
			}
			params, err := model.Decode[model.DeviceClassParameters](config.Opaque.Parameters.Raw)
			if err != nil {
				return nil, fmt.Errorf("DeviceClass %s: %w", class.Name, err)
			}
			if err := params.NormalizeAndValidate(); err != nil {
				return nil, fmt.Errorf("DeviceClass %s: %w", class.Name, err)
			}
			if previous, exists := profiles[params.Profile]; exists && !reflect.DeepEqual(previous, params) {
				return nil, fmt.Errorf("profile %q is defined differently by multiple DeviceClasses", params.Profile)
			}
			profiles[params.Profile] = params
		}
	}
	return profiles, nil
}

func (r *InventoryReconciler) reconcileNode(ctx context.Context, nodeName, instanceID, az string, profiles map[string]model.DeviceClassParameters) error {
	interfaces, err := r.EC2.DescribeNetworkInterfaces(ctx, &awsec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{{Name: stringptr("attachment.instance-id"), Values: []string{instanceID}}},
	})
	if err != nil {
		return fmt.Errorf("describe attached ENIs: %w", err)
	}
	subnetIDs := []string{}
	seenSubnets := map[string]bool{}
	for _, profile := range profiles {
		for _, path := range profile.Paths {
			if !seenSubnets[path.SubnetID] {
				seenSubnets[path.SubnetID] = true
				subnetIDs = append(subnetIDs, path.SubnetID)
			}
		}
	}
	subnetCIDRs := map[string]string{}
	if len(subnetIDs) > 0 {
		subnets, err := r.EC2.DescribeSubnets(ctx, &awsec2.DescribeSubnetsInput{SubnetIds: subnetIDs})
		if err != nil {
			return fmt.Errorf("describe carrier subnets: %w", err)
		}
		for _, subnet := range subnets.Subnets {
			if subnet.SubnetId != nil && subnet.CidrBlock != nil {
				subnetCIDRs[*subnet.SubnetId] = *subnet.CidrBlock
			}
		}
	}

	inventory := model.AnchorNodeInventorySpec{
		NodeName: nodeName, InstanceID: instanceID, AZ: az,
		Profiles: map[string][]model.ENIPath{}, SlotsPerNode: map[string]int{},
	}
	for name, profile := range profiles {
		paths := make([]model.ENIPath, 0, len(profile.Paths))
		complete := true
		for _, path := range profile.Paths {
			matches := matchingENIs(interfaces.NetworkInterfaces, path)
			if len(matches) != 1 {
				r.Logger.Warn("carrier path is not uniquely satisfied", "node", nodeName, "profile", name, "path", path.Name, "matches", len(matches))
				complete = false
				break
			}
			iface := matches[0]
			paths = append(paths, model.ENIPath{
				Name: path.Name, ENIID: value(iface.NetworkInterfaceId), MAC: value(iface.MacAddress),
				SubnetID: path.SubnetID, SubnetCIDR: subnetCIDRs[path.SubnetID], Tags: tags(iface.TagSet),
				PrimaryIPv4: primaryIPv4(iface.PrivateIpAddresses),
			})
		}
		if complete {
			inventory.Profiles[name] = paths
			inventory.SlotsPerNode[name] = profile.SlotsPerNode
		}
	}
	return r.upsertInventory(ctx, inventory)
}

func (r *InventoryReconciler) upsertInventory(ctx context.Context, spec model.AnchorNodeInventorySpec) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return err
	}
	resource := r.Dynamic.Resource(anchorkube.InventoryGVR)
	current, err := resource.Get(ctx, spec.NodeName, metav1.GetOptions{})
	if err == nil {
		old, _, _ := unstructured.NestedMap(current.Object, "spec")
		if reflect.DeepEqual(old, raw) {
			return nil
		}
		current.Object["spec"] = raw
		_, err = resource.Update(ctx, current, metav1.UpdateOptions{})
		return err
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.APIGroup + "/" + constants.APIVersion,
		"kind":       "AnchorNodeInventory",
		"metadata":   map[string]any{"name": spec.NodeName},
		"spec":       raw,
	}}
	_, err = resource.Create(ctx, object, metav1.CreateOptions{})
	return err
}

func matchingENIs(interfaces []ec2types.NetworkInterface, path model.PathSpec) []ec2types.NetworkInterface {
	var result []ec2types.NetworkInterface
	for _, iface := range interfaces {
		if value(iface.SubnetId) != path.SubnetID || iface.Attachment == nil || iface.Attachment.DeviceIndex == nil || *iface.Attachment.DeviceIndex == 0 {
			continue
		}
		if model.TagsMatch(tags(iface.TagSet), path.ENITagSelector) {
			result = append(result, iface)
		}
	}
	return result
}

func tags(values []ec2types.Tag) map[string]string {
	result := map[string]string{}
	for _, tag := range values {
		if tag.Key != nil && tag.Value != nil {
			result[*tag.Key] = *tag.Value
		}
	}
	return result
}

func primaryIPv4(values []ec2types.NetworkInterfacePrivateIpAddress) string {
	for _, address := range values {
		if address.Primary != nil && *address.Primary && address.PrivateIpAddress != nil {
			return *address.PrivateIpAddress
		}
	}
	return ""
}

func instanceID(providerID string) string {
	parts := strings.Split(providerID, "/")
	if len(parts) == 0 || !strings.HasPrefix(parts[len(parts)-1], "i-") {
		return ""
	}
	return parts[len(parts)-1]
}

func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}

func stringptr(value string) *string { return &value }
