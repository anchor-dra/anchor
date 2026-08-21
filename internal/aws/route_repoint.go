package aws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	anchormetrics "github.com/anchor-dra/anchor/internal/metrics"
	"github.com/anchor-dra/anchor/internal/model"
)

type RouteEC2API interface {
	DescribeNetworkInterfaces(context.Context, *awsec2.DescribeNetworkInterfacesInput, ...func(*awsec2.Options)) (*awsec2.DescribeNetworkInterfacesOutput, error)
	DescribeRouteTables(context.Context, *awsec2.DescribeRouteTablesInput, ...func(*awsec2.Options)) (*awsec2.DescribeRouteTablesOutput, error)
	DescribeVpcs(context.Context, *awsec2.DescribeVpcsInput, ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error)
	CreateRoute(context.Context, *awsec2.CreateRouteInput, ...func(*awsec2.Options)) (*awsec2.CreateRouteOutput, error)
	ReplaceRoute(context.Context, *awsec2.ReplaceRouteInput, ...func(*awsec2.Options)) (*awsec2.ReplaceRouteOutput, error)
}

type RouteRepoint struct {
	EC2             RouteEC2API
	ManagedTagKey   string
	ManagedTagValue string
	Logger          *slog.Logger
}

func NewRouteRepoint(client RouteEC2API, tagKey, tagValue string, logger *slog.Logger) *RouteRepoint {
	if logger == nil {
		logger = slog.Default()
	}
	return &RouteRepoint{EC2: client, ManagedTagKey: tagKey, ManagedTagValue: tagValue, Logger: logger}
}

type routeAction struct {
	tableID string
	create  bool
}

type routeSnapshot struct {
	endpoint Endpoint
	prefix   netip.Prefix
	tables   map[string]ec2types.RouteTable
	statuses []model.RouteTableStatus
	actions  []routeAction
}

func (s *RouteRepoint) Validate(ctx context.Context, endpoint Endpoint) error {
	_, err := s.snapshot(ctx, endpoint, true)
	return err
}

func (s *RouteRepoint) Place(ctx context.Context, endpoint Endpoint) error {
	result := s.PlaceBatch(ctx, []Endpoint{endpoint})
	if len(result) != 1 {
		return errors.New("placement returned no result")
	}
	return result[0].Err
}

func (s *RouteRepoint) PlaceBatch(ctx context.Context, endpoints []Endpoint) []PlacementResult {
	results := make([]PlacementResult, len(endpoints))
	failedClaims := map[string]bool{}
	for i, endpoint := range endpoints {
		if failedClaims[endpoint.ClaimUID] {
			results[i].Err = fmt.Errorf("route-repoint skipped path %q after an earlier path failed", endpoint.Path.Name)
			results[i].RouteTables = pendingRouteStatuses(endpoint, results[i].Err.Error())
			continue
		}
		results[i] = s.placeOne(ctx, endpoint)
		if results[i].Err != nil {
			failedClaims[endpoint.ClaimUID] = true
		}
	}
	return results
}

func (s *RouteRepoint) placeOne(ctx context.Context, endpoint Endpoint) PlacementResult {
	started := time.Now()
	timer := anchormetrics.PlacementDuration.WithLabelValues(model.StrategyRouteRepoint)
	defer func() { timer.Observe(time.Since(started).Seconds()) }()
	snapshot, err := s.snapshot(ctx, endpoint, true)
	if err != nil {
		anchormetrics.Placements.WithLabelValues(model.StrategyRouteRepoint, "error").Inc()
		return PlacementResult{Err: err, RouteTables: statusesFromSnapshot(snapshot)}
	}
	for _, action := range snapshot.actions {
		var callErr error
		if action.create {
			_, callErr = s.EC2.CreateRoute(ctx, &awsec2.CreateRouteInput{
				DestinationCidrBlock: awssdk.String(snapshot.prefix.String()), NetworkInterfaceId: awssdk.String(endpoint.Path.ENIID), RouteTableId: awssdk.String(action.tableID),
			})
		} else {
			_, callErr = s.EC2.ReplaceRoute(ctx, &awsec2.ReplaceRouteInput{
				DestinationCidrBlock: awssdk.String(snapshot.prefix.String()), NetworkInterfaceId: awssdk.String(endpoint.Path.ENIID), RouteTableId: awssdk.String(action.tableID),
			})
		}
		operation := "replace"
		if action.create {
			operation = "create"
		}
		result := "success"
		if callErr != nil {
			result = "error"
		}
		anchormetrics.RouteTableUpdates.WithLabelValues(operation, result).Inc()
		if callErr != nil {
			verified, verifyErr := s.snapshot(ctx, endpoint, false)
			if verifyErr == nil && allRoutesConverged(verified.statuses) {
				continue
			}
			anchormetrics.Placements.WithLabelValues(model.StrategyRouteRepoint, "error").Inc()
			code := apiErrorCode(callErr)
			if code == "" {
				code = "unknown"
			}
			message := fmt.Sprintf("%s route failed (%s): %v", operation, code, callErr)
			statuses := statusesFromSnapshot(verified)
			markRouteStatus(statuses, action.tableID, model.RouteTableError, message)
			return PlacementResult{Err: fmt.Errorf("%s route %s in %s (%s): %w", operation, snapshot.prefix, action.tableID, code, callErr), RouteTables: statuses}
		}
	}
	verified, err := s.snapshot(ctx, endpoint, false)
	if err != nil {
		anchormetrics.Placements.WithLabelValues(model.StrategyRouteRepoint, "error").Inc()
		return PlacementResult{Err: err, RouteTables: statusesFromSnapshot(verified)}
	}
	if !allRoutesConverged(verified.statuses) {
		err = fmt.Errorf("route-repoint for path %q has not converged in every managed table", endpoint.Path.Name)
		anchormetrics.Placements.WithLabelValues(model.StrategyRouteRepoint, "error").Inc()
		return PlacementResult{Err: err, RouteTables: verified.statuses}
	}
	anchormetrics.Placements.WithLabelValues(model.StrategyRouteRepoint, "success").Inc()
	s.Logger.Info("AWS endpoint routes converged", "claimUID", endpoint.ClaimUID, "claim", endpoint.ClaimName, "node", endpoint.NodeName, "eni", endpoint.Path.ENIID, "ip", snapshot.prefix.String(), "routeTables", endpoint.Path.RouteTableIDs, "latency", time.Since(started))
	return PlacementResult{RouteTables: verified.statuses}
}

func (s *RouteRepoint) VerifyBatch(ctx context.Context, endpoints []Endpoint) ([]VerificationResult, error) {
	results := make([]VerificationResult, len(endpoints))
	for i, endpoint := range endpoints {
		snapshot, err := s.snapshot(ctx, endpoint, false)
		results[i] = VerificationResult{Err: err, RouteTables: statusesFromSnapshot(snapshot)}
		results[i].Placed = err == nil && allRoutesConverged(results[i].RouteTables)
	}
	return results, nil
}

func (s *RouteRepoint) Release(context.Context, Endpoint) error { return nil }

func (s *RouteRepoint) snapshot(ctx context.Context, endpoint Endpoint, validateMutation bool) (*routeSnapshot, error) {
	snapshot := &routeSnapshot{endpoint: endpoint, tables: map[string]ec2types.RouteTable{}}
	if s.EC2 == nil {
		return snapshot, errors.New("EC2 client is required")
	}
	if s.ManagedTagKey == "" || s.ManagedTagValue == "" {
		return snapshot, errors.New("managed route-table tag key and value are required")
	}
	prefix, err := netip.ParsePrefix(endpoint.Path.IP)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 || prefix != prefix.Masked() {
		return snapshot, fmt.Errorf("route-repoint address %q must be a canonical IPv4 /32", endpoint.Path.IP)
	}
	snapshot.prefix = prefix
	tableIDs := unique(endpoint.Path.RouteTableIDs)
	if len(tableIDs) == 0 || len(tableIDs) != len(endpoint.Path.RouteTableIDs) {
		return snapshot, fmt.Errorf("path %q requires a unique, non-empty managed route-table set", endpoint.Path.Name)
	}
	sort.Strings(tableIDs)
	now := metav1Now()
	for _, tableID := range tableIDs {
		snapshot.statuses = append(snapshot.statuses, model.RouteTableStatus{PathName: endpoint.Path.Name, RouteTableID: tableID, Phase: model.RouteTablePending, LastAttemptAt: &now})
	}
	interfaces, err := s.EC2.DescribeNetworkInterfaces(ctx, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{endpoint.Path.ENIID}})
	if err != nil || len(interfaces.NetworkInterfaces) != 1 {
		if err == nil {
			err = fmt.Errorf("target ENI %s was not found", endpoint.Path.ENIID)
		}
		return snapshot, err
	}
	iface := interfaces.NetworkInterfaces[0]
	if iface.VpcId == nil || iface.SubnetId == nil || *iface.SubnetId != endpoint.Path.SubnetID {
		return snapshot, fmt.Errorf("target ENI %s does not match resolved VPC/subnet topology", endpoint.Path.ENIID)
	}
	if iface.SourceDestCheck == nil || *iface.SourceDestCheck {
		return snapshot, fmt.Errorf("target ENI %s must have source/destination check disabled", endpoint.Path.ENIID)
	}
	vpcID := *iface.VpcId
	vpcs, err := s.EC2.DescribeVpcs(ctx, &awsec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil || len(vpcs.Vpcs) != 1 {
		if err == nil {
			err = fmt.Errorf("VPC %s was not found", vpcID)
		}
		return snapshot, err
	}
	if vpcs.Vpcs[0].CidrBlock != nil {
		cidr, parseErr := netip.ParsePrefix(*vpcs.Vpcs[0].CidrBlock)
		if parseErr == nil && cidr.Contains(prefix.Addr()) {
			return snapshot, fmt.Errorf("route-repoint address %s is inside VPC CIDR %s", prefix, cidr)
		}
	}
	for _, association := range vpcs.Vpcs[0].CidrBlockAssociationSet {
		if association.CidrBlock == nil {
			continue
		}
		cidr, parseErr := netip.ParsePrefix(*association.CidrBlock)
		if parseErr == nil && cidr.Contains(prefix.Addr()) {
			return snapshot, fmt.Errorf("route-repoint address %s is inside VPC CIDR %s", prefix, cidr)
		}
	}
	output, err := s.EC2.DescribeRouteTables(ctx, &awsec2.DescribeRouteTablesInput{RouteTableIds: tableIDs})
	if err != nil {
		return snapshot, fmt.Errorf("describe managed route tables: %w", err)
	}
	for _, table := range output.RouteTables {
		if table.RouteTableId != nil {
			snapshot.tables[*table.RouteTableId] = table
		}
	}
	if len(snapshot.tables) != len(tableIDs) {
		return snapshot, fmt.Errorf("one or more managed route tables were not found")
	}
	var conflicts []error
	for i, tableID := range tableIDs {
		table := snapshot.tables[tableID]
		status := &snapshot.statuses[i]
		if table.VpcId == nil || *table.VpcId != vpcID {
			status.Phase, status.Message = model.RouteTableConflict, "route table is outside the target ENI VPC"
			conflicts = append(conflicts, fmt.Errorf("route table %s is outside VPC %s", tableID, vpcID))
			continue
		}
		if !tagMatches(table.Tags, s.ManagedTagKey, s.ManagedTagValue) {
			status.Phase, status.Message = model.RouteTableConflict, "route table lacks the required management tag"
			conflicts = append(conflicts, fmt.Errorf("route table %s lacks %s=%s", tableID, s.ManagedTagKey, s.ManagedTagValue))
			continue
		}
		route := findIPv4Route(table.Routes, prefix.String())
		if route == nil {
			status.Message = "exact /32 route is absent"
			if validateMutation {
				snapshot.actions = append(snapshot.actions, routeAction{tableID: tableID, create: true})
			}
			continue
		}
		status.ObservedTargetType, status.ObservedTargetID = routeTarget(*route)
		if route.NetworkInterfaceId != nil && *route.NetworkInterfaceId == endpoint.Path.ENIID && route.State == ec2types.RouteStateActive {
			status.Phase, status.Message = model.RouteTableConverged, "route targets the selected ENI"
			continue
		}
		if !validateMutation {
			status.Phase, status.Message = model.RouteTableConflict, "route does not target the selected ENI"
			continue
		}
		if route.NetworkInterfaceId == nil {
			status.Phase, status.Message = model.RouteTableConflict, "non-ENI route targets are never replaced"
			conflicts = append(conflicts, fmt.Errorf("route %s in %s targets %s %s, not an ENI", prefix, tableID, status.ObservedTargetType, status.ObservedTargetID))
			continue
		}
		owner := *route.NetworkInterfaceId
		if owner == endpoint.Path.ENIID {
			snapshot.actions = append(snapshot.actions, routeAction{tableID: tableID})
			continue
		}
		if owner != endpoint.PreviousENI && !endpoint.ForceSteal {
			status.Phase, status.Message = model.RouteTableConflict, "route targets an unowned ENI"
			conflicts = append(conflicts, fmt.Errorf("route %s in %s targets unowned ENI %s; set force-steal explicitly to override", prefix, tableID, owner))
			continue
		}
		snapshot.actions = append(snapshot.actions, routeAction{tableID: tableID})
	}
	return snapshot, errors.Join(conflicts...)
}

func pendingRouteStatuses(endpoint Endpoint, message string) []model.RouteTableStatus {
	now := metav1Now()
	statuses := make([]model.RouteTableStatus, 0, len(endpoint.Path.RouteTableIDs))
	for _, id := range endpoint.Path.RouteTableIDs {
		statuses = append(statuses, model.RouteTableStatus{PathName: endpoint.Path.Name, RouteTableID: id, Phase: model.RouteTablePending, LastAttemptAt: &now, Message: message})
	}
	return statuses
}

func statusesFromSnapshot(snapshot *routeSnapshot) []model.RouteTableStatus {
	if snapshot == nil {
		return nil
	}
	return snapshot.statuses
}

func markRouteStatus(statuses []model.RouteTableStatus, tableID, phase, message string) {
	for i := range statuses {
		if statuses[i].RouteTableID == tableID {
			statuses[i].Phase = phase
			statuses[i].Message = message
			return
		}
	}
}

func allRoutesConverged(statuses []model.RouteTableStatus) bool {
	if len(statuses) == 0 {
		return false
	}
	for _, status := range statuses {
		if status.Phase != model.RouteTableConverged {
			return false
		}
	}
	return true
}

func findIPv4Route(routes []ec2types.Route, destination string) *ec2types.Route {
	for i := range routes {
		if routes[i].DestinationCidrBlock != nil && *routes[i].DestinationCidrBlock == destination {
			return &routes[i]
		}
	}
	return nil
}

func routeTarget(route ec2types.Route) (string, string) {
	targets := []struct {
		kind  string
		value *string
	}{
		{"network-interface", route.NetworkInterfaceId}, {"transit-gateway", route.TransitGatewayId}, {"gateway", route.GatewayId},
		{"nat-gateway", route.NatGatewayId}, {"instance", route.InstanceId}, {"vpc-peering", route.VpcPeeringConnectionId},
		{"carrier-gateway", route.CarrierGatewayId}, {"local-gateway", route.LocalGatewayId}, {"egress-only-gateway", route.EgressOnlyInternetGatewayId},
		{"core-network", route.CoreNetworkArn},
	}
	for _, target := range targets {
		if target.value != nil && *target.value != "" {
			return target.kind, *target.value
		}
	}
	return "unknown", ""
}

func tagMatches(tags []ec2types.Tag, key, value string) bool {
	for _, tag := range tags {
		if tag.Key != nil && tag.Value != nil && *tag.Key == key && *tag.Value == value {
			return true
		}
	}
	return false
}

func apiErrorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

func metav1Now() metav1.Time { return metav1.Now() }

var _ BatchPlacementStrategy = (*RouteRepoint)(nil)
