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
