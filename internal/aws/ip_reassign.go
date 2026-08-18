package aws

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	anchormetrics "github.com/anchor-dra/anchor/internal/metrics"
	"github.com/anchor-dra/anchor/internal/model"
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
	results := s.PlaceBatch(ctx, []Endpoint{endpoint})
	if len(results) != 1 {
		return fmt.Errorf("placement returned no result")
	}
	return results[0]
}

func (s *IPReassign) PlaceBatch(ctx context.Context, endpoints []Endpoint) []error {
	results := make([]error, len(endpoints))
	if len(endpoints) == 0 {
		return results
	}
	targets, owners, err := s.snapshot(ctx, endpoints)
	if err != nil {
		for i := range results {
			results[i] = err
		}
		return results
	}
	for i := range endpoints {
		results[i] = s.placeFromSnapshot(ctx, endpoints[i], targets, owners)
	}
	return results
}

func (s *IPReassign) VerifyBatch(ctx context.Context, endpoints []Endpoint) ([]VerificationResult, error) {
	results := make([]VerificationResult, len(endpoints))
	if len(endpoints) == 0 {
		return results, nil
	}
	ips := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		ip, err := AddressOnly(endpoint.Path.IP)
		if err != nil {
			return nil, err
		}
		ips = append(ips, ip)
	}
	interfaces, err := s.describeByFilter(ctx, "addresses.private-ip-address", ips)
	if err != nil {
		return nil, fmt.Errorf("find current endpoint owners: %w", err)
	}
	owners := ownersByAddress(interfaces)
	for i, endpoint := range endpoints {
		ip, _ := AddressOnly(endpoint.Path.IP)
		matches := owners[ip]
		if len(matches) > 1 {
			results[i].Err = fmt.Errorf("address %s unexpectedly appears on %d ENIs", ip, len(matches))
			continue
		}
		if len(matches) == 0 {
			continue
		}
		if matches[0].primary {
			results[i].Err = fmt.Errorf("address %s is a primary ENI address and cannot be moved", ip)
			continue
		}
		results[i].Placed = matches[0].eniID == endpoint.Path.ENIID
	}
	return results, nil
}

type ownerInfo struct {
	eniID   string
	primary bool
}

func (s *IPReassign) snapshot(ctx context.Context, endpoints []Endpoint) (map[string]ec2types.NetworkInterface, map[string][]ownerInfo, error) {
	targetIDs := make([]string, 0, len(endpoints))
	ips := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		targetIDs = append(targetIDs, endpoint.Path.ENIID)
		ip, err := AddressOnly(endpoint.Path.IP)
		if err != nil {
			return nil, nil, err
		}
		ips = append(ips, ip)
	}
	targetInterfaces, err := s.describeByFilter(ctx, "network-interface-id", targetIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("describe target ENIs: %w", err)
	}
	ownerInterfaces, err := s.describeByFilter(ctx, "addresses.private-ip-address", ips)
	if err != nil {
		return nil, nil, fmt.Errorf("find current endpoint owners: %w", err)
	}
	targets := make(map[string]ec2types.NetworkInterface, len(targetInterfaces))
	for _, iface := range targetInterfaces {
		if iface.NetworkInterfaceId != nil {
			targets[*iface.NetworkInterfaceId] = iface
		}
	}
	return targets, ownersByAddress(ownerInterfaces), nil
}

func (s *IPReassign) placeFromSnapshot(ctx context.Context, endpoint Endpoint, targets map[string]ec2types.NetworkInterface, owners map[string][]ownerInfo) error {
	ip, _ := AddressOnly(endpoint.Path.IP)
	iface, found := targets[endpoint.Path.ENIID]
	if !found {
		return fmt.Errorf("target ENI %s was not found", endpoint.Path.ENIID)
	}
	if iface.SubnetId == nil || *iface.SubnetId != endpoint.Path.SubnetID {
		return fmt.Errorf("target ENI %s is not in subnet %s", endpoint.Path.ENIID, endpoint.Path.SubnetID)
	}
	matches := owners[ip]
	if len(matches) > 1 {
		return fmt.Errorf("address %s unexpectedly appears on %d ENIs", ip, len(matches))
	}
	owner := ""
	if len(matches) == 1 {
		owner = matches[0].eniID
		if matches[0].primary {
			return fmt.Errorf("address %s is a primary ENI address and cannot be moved", ip)
		}
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
	_, err := s.EC2.AssignPrivateIpAddresses(ctx, &awsec2.AssignPrivateIpAddressesInput{
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

func (s *IPReassign) describeByFilter(ctx context.Context, name string, values []string) ([]ec2types.NetworkInterface, error) {
	values = unique(values)
	result := []ec2types.NetworkInterface{}
	for start := 0; start < len(values); start += 200 {
		end := min(start+200, len(values))
		paginator := awsec2.NewDescribeNetworkInterfacesPaginator(s.EC2, &awsec2.DescribeNetworkInterfacesInput{
			Filters:    []ec2types.Filter{{Name: awssdk.String(name), Values: values[start:end]}},
			MaxResults: awssdk.Int32(1000),
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			result = append(result, page.NetworkInterfaces...)
		}
	}
	return result, nil
}

func ownersByAddress(interfaces []ec2types.NetworkInterface) map[string][]ownerInfo {
	result := map[string][]ownerInfo{}
	for _, iface := range interfaces {
		if iface.NetworkInterfaceId == nil {
			continue
		}
		for _, address := range iface.PrivateIpAddresses {
			if address.PrivateIpAddress == nil {
				continue
			}
			primary := address.Primary != nil && *address.Primary
			result[*address.PrivateIpAddress] = append(result[*address.PrivateIpAddress], ownerInfo{eniID: *iface.NetworkInterfaceId, primary: primary})
		}
	}
	return result
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
