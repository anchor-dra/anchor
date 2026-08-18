package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/anchor-dra/anchor/internal/constants"
	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/model"
)

type failingNodeDiscovery struct {
}

func (*failingNodeDiscovery) DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error) {
	return nil, errors.New("injected discovery failure")
}

type batchDiscovery struct {
	interfaces     []ec2types.NetworkInterface
	interfaceCalls int
	subnetCalls    int
}

func (f *batchDiscovery) DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error) {
	f.interfaceCalls++
	return &awsec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: f.interfaces}, nil
}

func (f *batchDiscovery) DescribeSubnets(context.Context, *awsec2.DescribeSubnetsInput, ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error) {
	f.subnetCalls++
	subnetID, cidr := "subnet-a", "10.0.1.0/24"
	return &awsec2.DescribeSubnetsOutput{Subnets: []ec2types.Subnet{{SubnetId: &subnetID, CidrBlock: &cidr}}}, nil
}

func (*failingNodeDiscovery) DescribeSubnets(context.Context, *awsec2.DescribeSubnetsInput, ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error) {
	return &awsec2.DescribeSubnetsOutput{}, nil
}

func TestInstanceID(t *testing.T) {
	if got := instanceID("aws:///eu-west-1a/i-012345"); got != "i-012345" {
		t.Fatalf("unexpected instance ID %q", got)
	}
	if got := instanceID(""); got != "" {
		t.Fatalf("unexpected empty provider result %q", got)
	}
}

func TestMatchingENIsRequiresSecondaryTagAndSubnet(t *testing.T) {
	secondary := int32(1)
	primary := int32(0)
	valueA, valueB := "a", "b"
	tagKey := "path"
	subnet := "subnet-a"
	interfaces := []ec2types.NetworkInterface{
		{NetworkInterfaceId: stringptr("eni-good"), SubnetId: &subnet, Attachment: &ec2types.NetworkInterfaceAttachment{DeviceIndex: &secondary}, TagSet: []ec2types.Tag{{Key: &tagKey, Value: &valueA}}},
		{NetworkInterfaceId: stringptr("eni-primary"), SubnetId: &subnet, Attachment: &ec2types.NetworkInterfaceAttachment{DeviceIndex: &primary}, TagSet: []ec2types.Tag{{Key: &tagKey, Value: &valueA}}},
		{NetworkInterfaceId: stringptr("eni-wrong-tag"), SubnetId: &subnet, Attachment: &ec2types.NetworkInterfaceAttachment{DeviceIndex: &secondary}, TagSet: []ec2types.Tag{{Key: &tagKey, Value: &valueB}}},
	}
	matches := matchingENIs(interfaces, model.PathSpec{Name: "a", SubnetID: subnet, ENITagSelector: model.TagSelector{"path": "a"}})
	if len(matches) != 1 || value(matches[0].NetworkInterfaceId) != "eni-good" {
		t.Fatalf("unexpected matches: %#v", matches)
	}
}

func TestInventoryFailureDoesNotBlockPlacement(t *testing.T) {
	ctx := context.Background()
	badNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bad"}, Spec: corev1.NodeSpec{ProviderID: "aws:///eu-west-1a/i-bad"}}
	uid := types.UID("ffffffff-ffff-ffff-ffff-ffffffffffff")
	claim := testClaim("test", "endpoint", uid)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.100/24", ENIID: "eni-new", Interface: "ens6", SubnetID: "subnet-a"}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: badNode.Name, Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, claim.Namespace, claim.Name, "eni-old")
	dynamicClient := testDynamicClient(placement, ownership)
	strategy := &recordingStrategy{}

	controller := &Controller{
		Inventory: &InventoryReconciler{Core: fake.NewSimpleClientset(badNode), Dynamic: dynamicClient, EC2: &failingNodeDiscovery{}},
		Placement: &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: strategy},
	}
	err := controller.Reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), "describe attached ENIs") {
		t.Fatalf("expected inventory error, got %v", err)
	}
	if len(strategy.endpoints) != 1 {
		t.Fatalf("placement was called %d times, want 1", len(strategy.endpoints))
	}
}

func TestInventoryDiscoveryIsBatchedForSixtyNodes(t *testing.T) {
	ctx := context.Background()
	objects := make([]runtime.Object, 0, 61)
	interfaces := make([]ec2types.NetworkInterface, 0, 60)
	secondary := int32(1)
	for i := 0; i < 60; i++ {
		instanceID := fmt.Sprintf("i-%03d", i)
		nodeName := fmt.Sprintf("node-%03d", i)
		objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}, Spec: corev1.NodeSpec{ProviderID: "aws:///eu-west-1a/" + instanceID}})
		eniID, subnetID, mac := fmt.Sprintf("eni-%03d", i), "subnet-a", fmt.Sprintf("02:00:00:00:00:%02x", i)
		tagKey, tagValue := "carrier-path", "a"
		interfaces = append(interfaces, ec2types.NetworkInterface{
			NetworkInterfaceId: &eniID, SubnetId: &subnetID, MacAddress: &mac,
			Attachment: &ec2types.NetworkInterfaceAttachment{InstanceId: &instanceID, DeviceIndex: &secondary},
			TagSet:     []ec2types.Tag{{Key: &tagKey, Value: &tagValue}},
		})
	}
	objects = append(objects, &resourceapi.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "carrier"},
		Spec: resourceapi.DeviceClassSpec{Config: []resourceapi.DeviceClassConfiguration{{DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{
			Driver:     constants.DriverName,
			Parameters: runtime.RawExtension{Raw: []byte(`{"strategy":"ip-reassign","profile":"carrier","paths":[{"name":"a","subnetId":"subnet-a","eniTagSelector":{"carrier-path":"a"}}]}`)},
		}}}}},
	})
	discovery := &batchDiscovery{interfaces: interfaces}
	dynamicClient := testDynamicClient()
	reconciler := &InventoryReconciler{Core: fake.NewSimpleClientset(objects...), Dynamic: dynamicClient, EC2: discovery}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if discovery.interfaceCalls != 1 || discovery.subnetCalls != 1 {
		t.Fatalf("inventory used %d ENI and %d subnet calls, want 1 each", discovery.interfaceCalls, discovery.subnetCalls)
	}
	if _, err := dynamicClient.Resource(anchorkube.InventoryGVR).Get(ctx, "node-059", metav1.GetOptions{}); err != nil {
		t.Fatalf("last node inventory was not created: %v", err)
	}
}
