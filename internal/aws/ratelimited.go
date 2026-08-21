package aws

import (
	"context"
	"fmt"
	"strings"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"golang.org/x/time/rate"

	anchormetrics "github.com/anchor-dra/anchor/internal/metrics"
)

// EC2Client is the complete AWS surface used by the controller. Wrapping this
// interface once gives inventory discovery and endpoint mutation one shared
// account-level request budget.
type EC2Client interface {
	DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error)
	DescribeSubnets(context.Context, *awsec2.DescribeSubnetsInput, ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error)
	DescribeRouteTables(context.Context, *awsec2.DescribeRouteTablesInput, ...func(*awsec2.Options)) (*awsec2.DescribeRouteTablesOutput, error)
	DescribeVpcs(context.Context, *awsec2.DescribeVpcsInput, ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error)
	AssignPrivateIpAddresses(context.Context, *awsec2.AssignPrivateIpAddressesInput, ...func(*awsec2.Options)) (*awsec2.AssignPrivateIpAddressesOutput, error)
	CreateRoute(context.Context, *awsec2.CreateRouteInput, ...func(*awsec2.Options)) (*awsec2.CreateRouteOutput, error)
	ReplaceRoute(context.Context, *awsec2.ReplaceRouteInput, ...func(*awsec2.Options)) (*awsec2.ReplaceRouteOutput, error)
}

type RateLimitedEC2 struct {
	client  EC2Client
	limiter *rate.Limiter
}

func NewRateLimitedEC2(client EC2Client, qps float64, burst int) *RateLimitedEC2 {
	if qps <= 0 {
		qps = 2
	}
	if burst <= 0 {
		burst = 2
	}
	return &RateLimitedEC2{client: client, limiter: rate.NewLimiter(rate.Limit(qps), burst)}
}

func (c *RateLimitedEC2) DescribeNetworkInterfaces(ctx context.Context, input *awsec2.DescribeNetworkInterfacesInput, options ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error) {
	if err := c.wait(ctx, "DescribeNetworkInterfaces"); err != nil {
		return nil, err
	}
	output, err := c.client.DescribeNetworkInterfaces(ctx, input, options...)
	c.observeThrottle("DescribeNetworkInterfaces", err)
	return output, err
}

func (c *RateLimitedEC2) DescribeSubnets(ctx context.Context, input *awsec2.DescribeSubnetsInput, options ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error) {
	if err := c.wait(ctx, "DescribeSubnets"); err != nil {
		return nil, err
	}
	output, err := c.client.DescribeSubnets(ctx, input, options...)
	c.observeThrottle("DescribeSubnets", err)
	return output, err
}

func (c *RateLimitedEC2) DescribeRouteTables(ctx context.Context, input *awsec2.DescribeRouteTablesInput, options ...func(*awsec2.Options)) (*awsec2.DescribeRouteTablesOutput, error) {
	if err := c.wait(ctx, "DescribeRouteTables"); err != nil {
		return nil, err
	}
	output, err := c.client.DescribeRouteTables(ctx, input, options...)
	c.observeThrottle("DescribeRouteTables", err)
	return output, err
}

func (c *RateLimitedEC2) DescribeVpcs(ctx context.Context, input *awsec2.DescribeVpcsInput, options ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error) {
	if err := c.wait(ctx, "DescribeVpcs"); err != nil {
		return nil, err
	}
	output, err := c.client.DescribeVpcs(ctx, input, options...)
	c.observeThrottle("DescribeVpcs", err)
	return output, err
}

func (c *RateLimitedEC2) AssignPrivateIpAddresses(ctx context.Context, input *awsec2.AssignPrivateIpAddressesInput, options ...func(*awsec2.Options)) (*awsec2.AssignPrivateIpAddressesOutput, error) {
	if err := c.wait(ctx, "AssignPrivateIpAddresses"); err != nil {
		return nil, err
	}
	output, err := c.client.AssignPrivateIpAddresses(ctx, input, options...)
	c.observeThrottle("AssignPrivateIpAddresses", err)
	return output, err
}

func (c *RateLimitedEC2) CreateRoute(ctx context.Context, input *awsec2.CreateRouteInput, options ...func(*awsec2.Options)) (*awsec2.CreateRouteOutput, error) {
	if err := c.wait(ctx, "CreateRoute"); err != nil {
		return nil, err
	}
	output, err := c.client.CreateRoute(ctx, input, options...)
	c.observeThrottle("CreateRoute", err)
	return output, err
}

func (c *RateLimitedEC2) ReplaceRoute(ctx context.Context, input *awsec2.ReplaceRouteInput, options ...func(*awsec2.Options)) (*awsec2.ReplaceRouteOutput, error) {
	if err := c.wait(ctx, "ReplaceRoute"); err != nil {
		return nil, err
	}
	output, err := c.client.ReplaceRoute(ctx, input, options...)
	c.observeThrottle("ReplaceRoute", err)
	return output, err
}

func (c *RateLimitedEC2) wait(ctx context.Context, operation string) error {
	if c.client == nil {
		return fmt.Errorf("EC2 client is required")
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("wait for EC2 API budget before %s: %w", operation, err)
	}
	return nil
}

func (c *RateLimitedEC2) observeThrottle(operation string, err error) {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "throttl") {
		anchormetrics.EC2Throttles.WithLabelValues(operation).Inc()
	}
}
