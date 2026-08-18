package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	anchorkube "github.com/anchor-dra/anchor/internal/kube"
	"github.com/anchor-dra/anchor/internal/model"
)

type failingNodeDiscovery struct {
	instanceID string
}

func (f *failingNodeDiscovery) DescribeNetworkInterfaces(_ context.Context, input *awsec2.DescribeNetworkInterfacesInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error) {
	for _, filter := range input.Filters {
		for _, instanceID := range filter.Values {
			if instanceID == f.instanceID {
				return nil, errors.New("injected discovery failure")
			}
		}
	}
	return &awsec2.DescribeNetworkInterfacesOutput{}, nil
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

func TestInventoryNodeFailureDoesNotBlockOtherNodesOrPlacement(t *testing.T) {
	ctx := context.Background()
	badNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bad"}, Spec: corev1.NodeSpec{ProviderID: "aws:///eu-west-1a/i-bad"}}
	goodNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "good"}, Spec: corev1.NodeSpec{ProviderID: "aws:///eu-west-1a/i-good"}}
	uid := types.UID("ffffffff-ffff-ffff-ffff-ffffffffffff")
	claim := testClaim("test", "endpoint", uid)
	path := model.PlacementPath{Name: "a", IP: "10.0.1.100/24", ENIID: "eni-new", Interface: "ens6", SubnetID: "subnet-a"}
	spec := model.EndpointPlacementSpec{ClaimName: claim.Name, ClaimUID: string(uid), NodeName: goodNode.Name, Strategy: model.StrategyIPReassign, Paths: []model.PlacementPath{path}}
	placement := testPlacement(t, claim.Namespace, "claim-"+string(uid), uid, spec, nil)
	ownership := testOwnership(t, path.IP, claim.Namespace, claim.Name, "eni-old")
	dynamicClient := testDynamicClient(placement, ownership)
	strategy := &recordingStrategy{}

	controller := &Controller{
		Inventory: &InventoryReconciler{Core: fake.NewSimpleClientset(badNode, goodNode), Dynamic: dynamicClient, EC2: &failingNodeDiscovery{instanceID: "i-bad"}},
		Placement: &PlacementReconciler{Core: fake.NewSimpleClientset(claim), Dynamic: dynamicClient, Strategy: strategy},
	}
	err := controller.Reconcile(ctx)
	if err == nil || !strings.Contains(err.Error(), "node bad") {
		t.Fatalf("expected bad-node inventory error, got %v", err)
	}
	if _, err := dynamicClient.Resource(anchorkube.InventoryGVR).Get(ctx, goodNode.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("good node inventory was not reconciled: %v", err)
	}
	if len(strategy.endpoints) != 1 {
		t.Fatalf("placement was called %d times, want 1", len(strategy.endpoints))
	}
}
