package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/anchor-dra/anchor/internal/model"
)

type fakeRouteEC2 struct {
	interfaceValue ec2types.NetworkInterface
	vpc            ec2types.Vpc
	tables         map[string]ec2types.RouteTable
	createCalls    int
	replaceCalls   int
	failCreateFor  string
}

func newFakeRouteEC2() *fakeRouteEC2 {
	sourceDestCheck := false
	return &fakeRouteEC2{
		interfaceValue: ec2types.NetworkInterface{NetworkInterfaceId: awssdk.String("eni-new"), SubnetId: awssdk.String("subnet-b"), VpcId: awssdk.String("vpc-a"), SourceDestCheck: &sourceDestCheck},
		vpc:            ec2types.Vpc{VpcId: awssdk.String("vpc-a"), CidrBlockAssociationSet: []ec2types.VpcCidrBlockAssociation{{CidrBlock: awssdk.String("10.0.0.0/16")}}},
		tables: map[string]ec2types.RouteTable{
			"rtb-a": managedTable("rtb-a"),
			"rtb-b": managedTable("rtb-b"),
		},
	}
}

func managedTable(id string) ec2types.RouteTable {
	return ec2types.RouteTable{RouteTableId: awssdk.String(id), VpcId: awssdk.String("vpc-a"), Tags: []ec2types.Tag{{Key: awssdk.String("anchor-managed"), Value: awssdk.String("true")}}}
}

func (f *fakeRouteEC2) DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error) {
	return &awsec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: []ec2types.NetworkInterface{f.interfaceValue}}, nil
}

func (f *fakeRouteEC2) DescribeVpcs(context.Context, *awsec2.DescribeVpcsInput, ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error) {
	return &awsec2.DescribeVpcsOutput{Vpcs: []ec2types.Vpc{f.vpc}}, nil
}

func (f *fakeRouteEC2) DescribeRouteTables(_ context.Context, input *awsec2.DescribeRouteTablesInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeRouteTablesOutput, error) {
	result := make([]ec2types.RouteTable, 0, len(input.RouteTableIds))
	for _, id := range input.RouteTableIds {
		if table, ok := f.tables[id]; ok {
			result = append(result, table)
		}
	}
	return &awsec2.DescribeRouteTablesOutput{RouteTables: result}, nil
}

func (f *fakeRouteEC2) CreateRoute(_ context.Context, input *awsec2.CreateRouteInput, _ ...func(*awsec2.Options)) (*awsec2.CreateRouteOutput, error) {
	f.createCalls++
	if f.failCreateFor == awssdk.ToString(input.RouteTableId) {
		f.failCreateFor = ""
		return nil, errors.New("injected create failure")
	}
	table := f.tables[awssdk.ToString(input.RouteTableId)]
	table.Routes = append(table.Routes, ec2types.Route{DestinationCidrBlock: input.DestinationCidrBlock, NetworkInterfaceId: input.NetworkInterfaceId, State: ec2types.RouteStateActive})
	f.tables[awssdk.ToString(input.RouteTableId)] = table
	return &awsec2.CreateRouteOutput{}, nil
}

func (f *fakeRouteEC2) ReplaceRoute(_ context.Context, input *awsec2.ReplaceRouteInput, _ ...func(*awsec2.Options)) (*awsec2.ReplaceRouteOutput, error) {
	f.replaceCalls++
	table := f.tables[awssdk.ToString(input.RouteTableId)]
	for i := range table.Routes {
		if awssdk.ToString(table.Routes[i].DestinationCidrBlock) == awssdk.ToString(input.DestinationCidrBlock) {
			table.Routes[i] = ec2types.Route{DestinationCidrBlock: input.DestinationCidrBlock, NetworkInterfaceId: input.NetworkInterfaceId, State: ec2types.RouteStateActive}
		}
	}
	f.tables[awssdk.ToString(input.RouteTableId)] = table
	return &awsec2.ReplaceRouteOutput{}, nil
}

func routeEndpoint() Endpoint {
	return Endpoint{ClaimUID: "claim-uid", ClaimName: "test/endpoint", NodeName: "node-b", PreviousENI: "eni-old", Path: model.PlacementPath{
		Name: "a", IP: "198.51.100.10/32", ENIID: "eni-new", SubnetID: "subnet-b", RouteTableIDs: []string{"rtb-a", "rtb-b"},
	}}
}

func TestRouteRepointCreatesCompleteSetAndIsIdempotent(t *testing.T) {
	fake := newFakeRouteEC2()
	strategy := NewRouteRepoint(fake, "anchor-managed", "true", nil)
	result := strategy.PlaceBatch(context.Background(), []Endpoint{routeEndpoint()})
	if result[0].Err != nil {
		t.Fatal(result[0].Err)
	}
	if fake.createCalls != 2 || !allRoutesConverged(result[0].RouteTables) {
		t.Fatalf("unexpected first placement: calls=%d status=%#v", fake.createCalls, result[0].RouteTables)
	}
	result = strategy.PlaceBatch(context.Background(), []Endpoint{routeEndpoint()})
	if result[0].Err != nil || fake.createCalls != 2 {
		t.Fatalf("idempotent placement mutated routes: calls=%d err=%v", fake.createCalls, result[0].Err)
	}
}

func TestRouteRepointRetainsPartialProgressAndRetries(t *testing.T) {
	fake := newFakeRouteEC2()
	fake.failCreateFor = "rtb-b"
	strategy := NewRouteRepoint(fake, "anchor-managed", "true", nil)
	first := strategy.PlaceBatch(context.Background(), []Endpoint{routeEndpoint()})[0]
	if first.Err == nil {
		t.Fatal("expected partial placement failure")
	}
	if route := findIPv4Route(fake.tables["rtb-a"].Routes, "198.51.100.10/32"); route == nil {
		t.Fatal("successful first-table update was rolled back")
	}
	if len(first.RouteTables) != 2 || first.RouteTables[0].Phase != model.RouteTableConverged || first.RouteTables[1].Phase != model.RouteTableError {
		t.Fatalf("partial route-table status is wrong: %#v", first.RouteTables)
	}
	second := strategy.PlaceBatch(context.Background(), []Endpoint{routeEndpoint()})[0]
	if second.Err != nil || !allRoutesConverged(second.RouteTables) {
		t.Fatalf("retry did not converge: %#v", second)
	}
}

func TestRouteRepointConflictAndForceStealBoundary(t *testing.T) {
	fake := newFakeRouteEC2()
	for id, table := range fake.tables {
		table.Routes = []ec2types.Route{{DestinationCidrBlock: awssdk.String("198.51.100.10/32"), NetworkInterfaceId: awssdk.String("eni-stranger"), State: ec2types.RouteStateActive}}
		fake.tables[id] = table
	}
	strategy := NewRouteRepoint(fake, "anchor-managed", "true", nil)
	endpoint := routeEndpoint()
	if err := strategy.Place(context.Background(), endpoint); err == nil {
		t.Fatal("unexpected ENI was overwritten without force-steal")
	}
	endpoint.ForceSteal = true
	if err := strategy.Place(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	if fake.replaceCalls != 2 {
		t.Fatalf("force-steal replaced %d routes, want 2", fake.replaceCalls)
	}

	table := fake.tables["rtb-a"]
	table.Routes[0] = ec2types.Route{DestinationCidrBlock: awssdk.String("198.51.100.10/32"), TransitGatewayId: awssdk.String("tgw-1"), State: ec2types.RouteStateActive}
	fake.tables["rtb-a"] = table
	if err := strategy.Place(context.Background(), endpoint); err == nil {
		t.Fatal("force-steal overwrote a non-ENI route")
	}
}

func TestRouteRepointRejectsAddressInsideVPC(t *testing.T) {
	fake := newFakeRouteEC2()
	endpoint := routeEndpoint()
	endpoint.Path.IP = "10.0.1.10/32"
	if err := NewRouteRepoint(fake, "anchor-managed", "true", nil).Place(context.Background(), endpoint); err == nil {
		t.Fatal("accepted route-repoint address inside VPC CIDR")
	}
}

func TestRouteRepointRepairsBlackholeOnDesiredENI(t *testing.T) {
	fake := newFakeRouteEC2()
	for id, table := range fake.tables {
		table.Routes = []ec2types.Route{{DestinationCidrBlock: awssdk.String("198.51.100.10/32"), NetworkInterfaceId: awssdk.String("eni-new"), State: ec2types.RouteStateBlackhole}}
		fake.tables[id] = table
	}
	if err := NewRouteRepoint(fake, "anchor-managed", "true", nil).Place(context.Background(), routeEndpoint()); err != nil {
		t.Fatal(err)
	}
	if fake.replaceCalls != 2 {
		t.Fatalf("repaired %d blackhole routes, want 2", fake.replaceCalls)
	}
}

var _ RouteEC2API = (*fakeRouteEC2)(nil)
