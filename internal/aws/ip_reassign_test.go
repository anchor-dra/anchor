package aws

import (
	"context"
	"errors"
	"fmt"
	"testing"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/anchor-dra/anchor/internal/model"
)

type fakeEC2 struct {
	interfaces []ec2types.NetworkInterface
	describes  int
	assigns    int
	assignErr  error
}

func (f *fakeEC2) DescribeNetworkInterfaces(_ context.Context, in *awsec2.DescribeNetworkInterfacesInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error) {
	f.describes++
	requested := map[string]bool{}
	for _, value := range in.Filters[0].Values {
		requested[value] = true
	}
	result := []ec2types.NetworkInterface{}
	for _, iface := range f.interfaces {
		matched := in.Filters[0].Name != nil && *in.Filters[0].Name == "network-interface-id" && iface.NetworkInterfaceId != nil && requested[*iface.NetworkInterfaceId]
		if !matched {
			for _, address := range iface.PrivateIpAddresses {
				if address.PrivateIpAddress != nil && requested[*address.PrivateIpAddress] {
					matched = true
				}
			}
		}
		if matched {
			result = append(result, iface)
		}
	}
	return &awsec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: result}, nil
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

func TestPlaceBatchUsesTwoDescribeCalls(t *testing.T) {
	fake := &fakeEC2{}
	strategy := NewIPReassign(fake, nil)
	endpoints := make([]Endpoint, 0, 20)
	for i := 1; i <= 20; i++ {
		eniID, subnetID, ip := fmt.Sprintf("eni-%d", i), fmt.Sprintf("subnet-%d", i), fmt.Sprintf("10.0.%d.10", i)
		fake.interfaces = append(fake.interfaces, iface(eniID, subnetID))
		endpoints = append(endpoints, Endpoint{ClaimUID: fmt.Sprintf("uid-%d", i), Path: model.PlacementPath{Name: "a", IP: ip + "/24", ENIID: eniID, SubnetID: subnetID}})
	}
	results := strategy.PlaceBatch(context.Background(), endpoints)
	for _, result := range results {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	if fake.describes != 2 {
		t.Fatalf("batch used %d Describe calls, want 2", fake.describes)
	}
	if fake.assigns != 20 {
		t.Fatalf("batch used %d assignments, want 20", fake.assigns)
	}
}

func TestVerifyBatchUsesOneDescribeCall(t *testing.T) {
	fake := &fakeEC2{}
	endpoints := make([]Endpoint, 0, 20)
	for i := 1; i <= 20; i++ {
		eniID, subnetID, ip := fmt.Sprintf("eni-%d", i), fmt.Sprintf("subnet-%d", i), fmt.Sprintf("10.1.%d.10", i)
		fake.interfaces = append(fake.interfaces, iface(eniID, subnetID, ip))
		endpoints = append(endpoints, Endpoint{Path: model.PlacementPath{IP: ip + "/24", ENIID: eniID, SubnetID: subnetID}})
	}
	results, err := NewIPReassign(fake, nil).VerifyBatch(context.Background(), endpoints)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.Err != nil || !result.Placed {
			t.Fatalf("unexpected verification result: %#v", result)
		}
	}
	if fake.describes != 1 {
		t.Fatalf("verification used %d Describe calls, want 1", fake.describes)
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
