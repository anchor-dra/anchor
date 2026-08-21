package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/anchor-dra/anchor/internal/constants"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/model"
)

type DiscoveryAPI interface {
	DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error)
	DescribeSubnets(context.Context, *awsec2.DescribeSubnetsInput, ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error)
}

type InventoryReconciler struct {
	Core                 kubernetes.Interface
	Dynamic              dynamic.Interface
	EC2                  DiscoveryAPI
	Platform             string
	NodeLabel            string
	EnabledStrategies    map[string]bool
	AdvertisedStrategies map[string]bool
	Logger               *slog.Logger
}

func (r *InventoryReconciler) Reconcile(ctx context.Context) error {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	profiles, err := r.profiles(ctx)
	if err != nil {
		return err
	}
	nodes, err := r.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: r.NodeLabel})
	if err != nil {
		return fmt.Errorf("list enabled nodes: %w", err)
	}
	if r.Platform == "onprem" {
		var nodeErrors []error
		for i := range nodes.Items {
			node := &nodes.Items[i]
			inventory := r.inventoryForOnPremNode(node, profiles)
			if err := r.upsertInventory(ctx, inventory); err != nil {
				nodeErrors = append(nodeErrors, fmt.Errorf("reconcile inventory for node %s: %w", node.Name, err))
				continue
			}
			if err := r.syncNodeReadinessTaint(ctx, node.Name); err != nil {
				nodeErrors = append(nodeErrors, fmt.Errorf("reconcile readiness taint for node %s: %w", node.Name, err))
			}
		}
		return errors.Join(nodeErrors...)
	}
	if r.EC2 == nil {
		return errors.New("AWS inventory requires an EC2 discovery client")
	}
	nodesByInstance := map[string]*coreNode{}
	instanceIDs := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		instanceID := instanceID(node.Spec.ProviderID)
		if instanceID == "" {
			r.Logger.Warn("enabled node has no AWS provider ID", "node", node.Name)
			continue
		}
		nodesByInstance[instanceID] = &coreNode{name: node.Name, az: node.Labels["topology.kubernetes.io/zone"]}
		instanceIDs = append(instanceIDs, instanceID)
	}
	interfaces, err := r.describeInterfaces(ctx, instanceIDs)
	if err != nil {
		return fmt.Errorf("describe attached ENIs: %w", err)
	}
	subnetCIDRs, err := r.describeSubnets(ctx, profileSubnetIDs(profiles))
	if err != nil {
		return fmt.Errorf("describe carrier subnets: %w", err)
	}
	interfacesByInstance := map[string][]ec2types.NetworkInterface{}
	for _, iface := range interfaces {
		if iface.Attachment != nil && iface.Attachment.InstanceId != nil {
			interfacesByInstance[*iface.Attachment.InstanceId] = append(interfacesByInstance[*iface.Attachment.InstanceId], iface)
		}
	}
	var nodeErrors []error
	for instanceID, node := range nodesByInstance {
		inventory := r.inventoryForNode(node.name, instanceID, node.az, interfacesByInstance[instanceID], subnetCIDRs, profiles)
		if err := r.upsertInventory(ctx, inventory); err != nil {
			nodeErrors = append(nodeErrors, fmt.Errorf("reconcile inventory for node %s: %w", node.name, err))
			continue
		}
		if err := r.syncNodeReadinessTaint(ctx, node.name); err != nil {
			nodeErrors = append(nodeErrors, fmt.Errorf("reconcile readiness taint for node %s: %w", node.name, err))
		}
	}
	return errors.Join(nodeErrors...)
}

func (r *InventoryReconciler) inventoryForOnPremNode(node *corev1.Node, profiles map[string]model.DeviceClassParameters) model.AnchorNodeInventorySpec {
	inventory := model.AnchorNodeInventorySpec{
		NodeName: node.Name, InstanceID: node.Name, AZ: node.Labels["topology.kubernetes.io/zone"],
		Profiles: map[string][]model.ENIPath{}, SlotsPerNode: map[string]int{},
	}
	for name, profile := range profiles {
		if profile.Strategy != model.StrategyL2Announce {
			continue
		}
		paths := make([]model.ENIPath, 0, len(profile.Paths))
		for _, path := range profile.Paths {
			paths = append(paths, model.ENIPath{
				Name: path.Name, ENIID: node.Name + "/" + path.ParentInterface,
				SubnetID: "onprem/" + name + "/" + path.Name, SubnetCIDR: path.SubnetCIDR,
				Interface: path.ParentInterface,
			})
		}
		inventory.Profiles[name] = paths
		if r.strategyAdvertised(profile.Strategy) {
			inventory.SlotsPerNode[name] = profile.SlotsPerNode
		}
	}
	return inventory
}

func (r *InventoryReconciler) ReconcileTaints(ctx context.Context) error {
	nodes, err := r.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: r.NodeLabel})
	if err != nil {
		return fmt.Errorf("list enabled nodes for readiness taints: %w", err)
	}
	var errs []error
	for i := range nodes.Items {
		if err := r.syncNodeReadinessTaint(ctx, nodes.Items[i].Name); err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", nodes.Items[i].Name, err))
		}
	}
	return errors.Join(errs...)
}

type coreNode struct {
	name string
	az   string
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
			if !r.strategyEnabled(params.Strategy) {
				r.Logger.Warn("DeviceClass strategy is not enabled; profile will not be advertised", "deviceClass", class.Name, "profile", params.Profile, "strategy", params.Strategy)
				continue
			}
			if previous, exists := profiles[params.Profile]; exists && !reflect.DeepEqual(previous, params) {
				return nil, fmt.Errorf("profile %q is defined differently by multiple DeviceClasses", params.Profile)
			}
			profiles[params.Profile] = params
		}
	}
	return profiles, nil
}

func (r *InventoryReconciler) strategyEnabled(strategy string) bool {
	if r.EnabledStrategies == nil {
		return strategy == model.StrategyIPReassign
	}
	return r.EnabledStrategies[strategy]
}

func (r *InventoryReconciler) strategyAdvertised(strategy string) bool {
	if r.AdvertisedStrategies == nil {
		return r.strategyEnabled(strategy)
	}
	return r.AdvertisedStrategies[strategy]
}

func (r *InventoryReconciler) syncNodeReadinessTaint(ctx context.Context, nodeName string) error {
	inventory, err := r.Dynamic.Resource(anchorkube.InventoryGVR).Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	var status model.AnchorNodeInventoryStatus
	ready := false
	if inventory != nil {
		rawStatus, _, _ := unstructured.NestedMap(inventory.Object, "status")
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawStatus, &status); err != nil {
			return err
		}
		ready = status.Ready && status.ObservedGeneration == inventory.GetGeneration()
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := r.Core.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		taints := make([]corev1.Taint, 0, len(node.Spec.Taints)+1)
		found := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == constants.NotReadyTaintKey {
				found = true
				if ready {
					continue
				}
			}
			taints = append(taints, taint)
		}
		if !ready && !found {
			taints = append(taints, corev1.Taint{Key: constants.NotReadyTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule})
		}
		if reflect.DeepEqual(node.Spec.Taints, taints) {
			return nil
		}
		copy := node.DeepCopy()
		copy.Spec.Taints = taints
		_, err = r.Core.CoreV1().Nodes().Update(ctx, copy, metav1.UpdateOptions{})
		return err
	})
}

func (r *InventoryReconciler) inventoryForNode(nodeName, instanceID, az string, interfaces []ec2types.NetworkInterface, subnetCIDRs map[string]string, profiles map[string]model.DeviceClassParameters) model.AnchorNodeInventorySpec {
	inventory := model.AnchorNodeInventorySpec{
		NodeName: nodeName, InstanceID: instanceID, AZ: az,
		Profiles: map[string][]model.ENIPath{}, SlotsPerNode: map[string]int{},
	}
	for name, profile := range profiles {
		paths := make([]model.ENIPath, 0, len(profile.Paths))
		complete := true
		for _, path := range profile.Paths {
			matches := matchingENIs(interfaces, path)
			if len(matches) != 1 {
				r.Logger.Warn("carrier path is not uniquely satisfied", "node", nodeName, "profile", name, "path", path.Name, "matches", len(matches))
				complete = false
				break
			}
			iface := matches[0]
			if profile.Strategy == model.StrategyRouteRepoint && (iface.SourceDestCheck == nil || *iface.SourceDestCheck) {
				r.Logger.Warn("route-repoint carrier ENI has source/destination check enabled", "node", nodeName, "profile", name, "path", path.Name, "eni", value(iface.NetworkInterfaceId))
				complete = false
				break
			}
			subnetID := value(iface.SubnetId)
			paths = append(paths, model.ENIPath{
				Name: path.Name, ENIID: value(iface.NetworkInterfaceId), MAC: value(iface.MacAddress),
				SubnetID: subnetID, SubnetCIDR: subnetCIDRs[subnetID], Tags: tags(iface.TagSet),
				PrimaryIPv4: primaryIPv4(iface.PrivateIpAddresses),
			})
		}
		if complete {
			inventory.Profiles[name] = paths
			if r.strategyAdvertised(profile.Strategy) {
				inventory.SlotsPerNode[name] = profile.SlotsPerNode
			}
		}
	}
	return inventory
}

func (r *InventoryReconciler) describeInterfaces(ctx context.Context, instanceIDs []string) ([]ec2types.NetworkInterface, error) {
	result := []ec2types.NetworkInterface{}
	for _, values := range chunks(uniqueStrings(instanceIDs), 200) {
		paginator := awsec2.NewDescribeNetworkInterfacesPaginator(r.EC2, &awsec2.DescribeNetworkInterfacesInput{
			Filters:    []ec2types.Filter{{Name: awssdk.String("attachment.instance-id"), Values: values}},
			MaxResults: awssdk.Int32(1000),
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			result = append(result, page.NetworkInterfaces...)
		}
	}
	return result, nil
}

func (r *InventoryReconciler) describeSubnets(ctx context.Context, subnetIDs []string) (map[string]string, error) {
	result := map[string]string{}
	for _, values := range chunks(uniqueStrings(subnetIDs), 200) {
		paginator := awsec2.NewDescribeSubnetsPaginator(r.EC2, &awsec2.DescribeSubnetsInput{
			Filters:    []ec2types.Filter{{Name: awssdk.String("subnet-id"), Values: values}},
			MaxResults: awssdk.Int32(1000),
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, subnet := range page.Subnets {
				if subnet.SubnetId != nil && subnet.CidrBlock != nil {
					result[*subnet.SubnetId] = *subnet.CidrBlock
				}
			}
		}
	}
	return result, nil
}

func profileSubnetIDs(profiles map[string]model.DeviceClassParameters) []string {
	result := []string{}
	for _, profile := range profiles {
		for _, path := range profile.Paths {
			if len(path.Subnets) == 0 {
				result = append(result, path.SubnetID)
				continue
			}
			for _, subnet := range path.Subnets {
				result = append(result, subnet.SubnetID)
			}
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func chunks(values []string, size int) [][]string {
	result := [][]string{}
	for start := 0; start < len(values); start += size {
		result = append(result, values[start:min(start+size, len(values))])
	}
	return result
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
		if _, allowed := path.ResolveAWSSubnet(value(iface.SubnetId)); !allowed || iface.Attachment == nil || iface.Attachment.DeviceIndex == nil || *iface.Attachment.DeviceIndex == 0 {
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
