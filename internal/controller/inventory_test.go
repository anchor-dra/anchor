package controller

import (
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/anchor-dra/anchor/internal/model"
)

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
