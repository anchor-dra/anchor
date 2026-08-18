package aws

import (
	"context"
	"errors"
	"testing"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/anchor-dra/anchor/internal/model"
)

type fakeEC2 struct {
	interfaces []ec2types.NetworkInterface
	assigns    int
	assignErr  error
}

func (f *fakeEC2) DescribeNetworkInterfaces(_ context.Context, in *awsec2.DescribeNetworkInterfacesInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error) {
	if len(in.NetworkInterfaceIds) > 0 {
		for _, iface := range f.interfaces {
			if iface.NetworkInterfaceId != nil && *iface.NetworkInterfaceId == in.NetworkInterfaceIds[0] {
				return &awsec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: []ec2types.NetworkInterface{iface}}, nil
			}
		}
		return &awsec2.DescribeNetworkInterfacesOutput{}, nil
	}
	ip := in.Filters[0].Values[0]
	for _, iface := range f.interfaces {
		for _, address := range iface.PrivateIpAddresses {
			if address.PrivateIpAddress != nil && *address.PrivateIpAddress == ip {
				return &awsec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: []ec2types.NetworkInterface{iface}}, nil
			}
		}
	}
	return &awsec2.DescribeNetworkInterfacesOutput{}, nil
}

func (f *fakeEC2) AssignPrivateIpAddresses(_ context.Context, in *awsec2.AssignPrivateIpAddressesInput, _ ...func(*awsec2.Options)) (*awsec2.AssignPrivateIpAddressesOutput, error) {
	f.assigns++
	if f.assignErr != nil {
		return nil, f.assignErr
	}
	for i := range f.interfaces {
		if f.interfaces[i].NetworkInterfaceId != nil && *f.interfaces[i].NetworkInterfaceId == *in.NetworkInterfaceId {
			f.interfaces[i].PrivateIpAddresses = append(f.interfaces[i].PrivateIpAddresses, ec2types.NetworkInterfacePrivateIpAddress{PrivateIpAddress: &in.PrivateIpAddresses[0]})
		}
	}
	return &awsec2.AssignPrivateIpAddressesOutput{}, nil
}

func endpoint() Endpoint {
	return Endpoint{ClaimUID: "uid", ClaimName: "ns/claim", NodeName: "node-b", Path: model.PlacementPath{Name: "a", IP: "10.0.1.10/24", ENIID: "eni-target", SubnetID: "subnet-a"}}
}

func iface(id, subnet string, ips ...string) ec2types.NetworkInterface {
	values := make([]ec2types.NetworkInterfacePrivateIpAddress, 0, len(ips))
	for _, ip := range ips {
		copy := ip
		values = append(values, ec2types.NetworkInterfacePrivateIpAddress{PrivateIpAddress: &copy})
	}
	return ec2types.NetworkInterface{NetworkInterfaceId: &id, SubnetId: &subnet, PrivateIpAddresses: values}
}

func TestPlaceNewAndIdempotent(t *testing.T) {
	fake := &fakeEC2{interfaces: []ec2types.NetworkInterface{iface("eni-target", "subnet-a")}}
	strategy := NewIPReassign(fake, nil)
	if err := strategy.Place(context.Background(), endpoint()); err != nil {
		t.Fatal(err)
	}
	if err := strategy.Place(context.Background(), endpoint()); err != nil {
		t.Fatal(err)
	}
	if fake.assigns != 1 {
		t.Fatalf("expected one mutation, got %d", fake.assigns)
	}
}

func TestRefusesUnownedAssignment(t *testing.T) {
	fake := &fakeEC2{interfaces: []ec2types.NetworkInterface{iface("eni-target", "subnet-a"), iface("eni-external", "subnet-a", "10.0.1.10")}}
	strategy := NewIPReassign(fake, nil)
	if err := strategy.Place(context.Background(), endpoint()); err == nil {
		t.Fatal("expected conflict")
	}
	if fake.assigns != 0 {
		t.Fatal("conflict caused a mutation")
	}
}

func TestAllowsPreviousOrForcedAssignment(t *testing.T) {
	for _, force := range []bool{false, true} {
		fake := &fakeEC2{interfaces: []ec2types.NetworkInterface{iface("eni-target", "subnet-a"), iface("eni-old", "subnet-a", "10.0.1.10")}}
		value := endpoint()
		value.ForceSteal = force
		if !force {
			value.PreviousENI = "eni-old"
		}
		if err := NewIPReassign(fake, nil).Place(context.Background(), value); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAssignFailureIsReturned(t *testing.T) {
	fake := &fakeEC2{interfaces: []ec2types.NetworkInterface{iface("eni-target", "subnet-a")}, assignErr: errors.New("throttled")}
	if err := NewIPReassign(fake, nil).Place(context.Background(), endpoint()); err == nil {
		t.Fatal("expected assignment error")
	}
}
