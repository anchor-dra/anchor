package aws

import (
	"context"
	"testing"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
)

type countingEC2 struct {
	describes int
}

func (c *countingEC2) DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error) {
	c.describes++
	return &awsec2.DescribeNetworkInterfacesOutput{}, nil
}

func (c *countingEC2) DescribeSubnets(context.Context, *awsec2.DescribeSubnetsInput, ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error) {
	return &awsec2.DescribeSubnetsOutput{}, nil
}

func (c *countingEC2) DescribeRouteTables(context.Context, *awsec2.DescribeRouteTablesInput, ...func(*awsec2.Options)) (*awsec2.DescribeRouteTablesOutput, error) {
	return &awsec2.DescribeRouteTablesOutput{}, nil
}
func (c *countingEC2) DescribeVpcs(context.Context, *awsec2.DescribeVpcsInput, ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error) {
	return &awsec2.DescribeVpcsOutput{}, nil
}
func (c *countingEC2) CreateRoute(context.Context, *awsec2.CreateRouteInput, ...func(*awsec2.Options)) (*awsec2.CreateRouteOutput, error) {
	return &awsec2.CreateRouteOutput{}, nil
}
func (c *countingEC2) ReplaceRoute(context.Context, *awsec2.ReplaceRouteInput, ...func(*awsec2.Options)) (*awsec2.ReplaceRouteOutput, error) {
	return &awsec2.ReplaceRouteOutput{}, nil
}

func (c *countingEC2) AssignPrivateIpAddresses(context.Context, *awsec2.AssignPrivateIpAddressesInput, ...func(*awsec2.Options)) (*awsec2.AssignPrivateIpAddressesOutput, error) {
	return &awsec2.AssignPrivateIpAddressesOutput{}, nil
}

func TestRateLimiterStopsCallWhenContextIsCancelled(t *testing.T) {
	fake := &countingEC2{}
	client := NewRateLimitedEC2(fake, 1, 1)
	if _, err := client.DescribeNetworkInterfaces(context.Background(), &awsec2.DescribeNetworkInterfacesInput{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.DescribeNetworkInterfaces(ctx, &awsec2.DescribeNetworkInterfacesInput{}); err == nil {
		t.Fatal("expected cancelled request to be rejected by limiter")
	}
	if fake.describes != 1 {
		t.Fatalf("cancelled call reached EC2 client; calls=%d", fake.describes)
	}
}
