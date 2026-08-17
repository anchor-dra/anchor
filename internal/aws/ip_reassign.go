package aws

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	anchormetrics "example.com/anchor/internal/metrics"
	"example.com/anchor/internal/model"
)

type EC2API interface {
	DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error)
	AssignPrivateIpAddresses(context.Context, *awsec2.AssignPrivateIpAddressesInput, ...func(*awsec2.Options)) (*awsec2.AssignPrivateIpAddressesOutput, error)
}

type IPReassign struct {
	EC2    EC2API
	Logger *slog.Logger
}

func NewIPReassign(client EC2API, logger *slog.Logger) *IPReassign {
	if logger == nil {
		logger = slog.Default()
	}
	return &IPReassign{EC2: client, Logger: logger}
}

func (s *IPReassign) Validate(ctx context.Context, endpoint Endpoint) error {
	if s.EC2 == nil {
		return fmt.Errorf("EC2 client is required")
	}
	ip, err := AddressOnly(endpoint.Path.IP)
	if err != nil {
		return err
	}
	output, err := s.EC2.DescribeNetworkInterfaces(ctx, &awsec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []string{endpoint.Path.ENIID},
	})
	if err != nil {
		return fmt.Errorf("describe target ENI %s: %w", endpoint.Path.ENIID, err)
	}
	if len(output.NetworkInterfaces) != 1 {
		return fmt.Errorf("target ENI %s was not found", endpoint.Path.ENIID)
	}
	iface := output.NetworkInterfaces[0]
	if iface.SubnetId == nil || *iface.SubnetId != endpoint.Path.SubnetID {
		return fmt.Errorf("target ENI %s is not in subnet %s", endpoint.Path.ENIID, endpoint.Path.SubnetID)
	}
	for _, address := range iface.PrivateIpAddresses {
		if address.PrivateIpAddress != nil && *address.PrivateIpAddress == ip && address.Primary != nil && *address.Primary {
			return fmt.Errorf("address %s is the primary IP of ENI %s", ip, endpoint.Path.ENIID)
		}
	}
	return nil
}

func (s *IPReassign) Place(ctx context.Context, endpoint Endpoint) error {
	if err := s.Validate(ctx, endpoint); err != nil {
		return err
	}
	ip, _ := AddressOnly(endpoint.Path.IP)
	owner, primary, err := s.currentOwner(ctx, ip)
	if err != nil {
		return err
	}
	if primary {
		return fmt.Errorf("address %s is a primary ENI address and cannot be moved", ip)
	}
	if owner == endpoint.Path.ENIID {
		s.Logger.Info("AWS endpoint already placed", "claim", endpoint.ClaimName, "node", endpoint.NodeName, "eni", owner, "ip", ip)
		return nil
	}
	if owner != "" && owner != endpoint.PreviousENI && !endpoint.ForceSteal {
		return fmt.Errorf("address %s is assigned to unowned ENI %s; set force-steal explicitly to override", ip, owner)
	}
	started := time.Now()
	timer := anchormetrics.PlacementDuration.WithLabelValues(model.StrategyIPReassign)
	defer func() {
		timer.Observe(time.Since(started).Seconds())
	}()
	allow := true
	_, err = s.EC2.AssignPrivateIpAddresses(ctx, &awsec2.AssignPrivateIpAddressesInput{
		AllowReassignment:  &allow,
		NetworkInterfaceId: &endpoint.Path.ENIID,
		PrivateIpAddresses: []string{ip},
	})
	if err != nil {
		anchormetrics.Placements.WithLabelValues(model.StrategyIPReassign, "error").Inc()
		return fmt.Errorf("assign %s to ENI %s: %w", ip, endpoint.Path.ENIID, err)
	}
	anchormetrics.Placements.WithLabelValues(model.StrategyIPReassign, "success").Inc()
	s.Logger.Info("AWS endpoint placed", "claimUID", endpoint.ClaimUID, "claim", endpoint.ClaimName, "node", endpoint.NodeName, "eni", endpoint.Path.ENIID, "previousENI", owner, "ip", ip, "latency", time.Since(started))
	return nil
}

func (s *IPReassign) Release(context.Context, Endpoint) error {
	// Deliberately retained. The next Place call atomically reassigns the IP.
	return nil
}

func (s *IPReassign) currentOwner(ctx context.Context, ip string) (string, bool, error) {
	output, err := s.EC2.DescribeNetworkInterfaces(ctx, &awsec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{{Name: strptr("addresses.private-ip-address"), Values: []string{ip}}},
	})
	if err != nil {
		return "", false, fmt.Errorf("find current owner for %s: %w", ip, err)
	}
	if len(output.NetworkInterfaces) > 1 {
		return "", false, fmt.Errorf("address %s unexpectedly appears on %d ENIs", ip, len(output.NetworkInterfaces))
	}
	if len(output.NetworkInterfaces) == 0 || output.NetworkInterfaces[0].NetworkInterfaceId == nil {
		return "", false, nil
	}
	iface := output.NetworkInterfaces[0]
	primary := false
	for _, address := range iface.PrivateIpAddresses {
		if address.PrivateIpAddress != nil && *address.PrivateIpAddress == ip && address.Primary != nil {
			primary = *address.Primary
		}
	}
	return *iface.NetworkInterfaceId, primary, nil
}

func strptr(value string) *string { return &value }
