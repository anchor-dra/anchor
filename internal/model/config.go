package model

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const (
	StrategyL2Announce   = "l2-announce"
	StrategyIPReassign   = "ip-reassign"
	StrategyRouteRepoint = "route-repoint"
)

type TagSelector map[string]string

type AWSSubnetSpec struct {
	SubnetID string `json:"subnetId" yaml:"subnetId"`
	Gateway  string `json:"gateway,omitempty" yaml:"gateway,omitempty"`
}

type PathSpec struct {
	Name            string          `json:"name" yaml:"name"`
	InterfaceName   string          `json:"interfaceName" yaml:"interfaceName"`
	RoutingTable    int             `json:"routingTable" yaml:"routingTable"`
	SubnetID        string          `json:"subnetId" yaml:"subnetId"`
	SubnetCIDR      string          `json:"subnetCidr,omitempty" yaml:"subnetCidr,omitempty"`
	ParentInterface string          `json:"parentInterface,omitempty" yaml:"parentInterface,omitempty"`
	ENITagSelector  TagSelector     `json:"eniTagSelector" yaml:"eniTagSelector"`
	Subnets         []AWSSubnetSpec `json:"subnets,omitempty" yaml:"subnets,omitempty"`
	RouteTableIDs   []string        `json:"routeTableIds,omitempty" yaml:"routeTableIds,omitempty"`
	Gateway         string          `json:"gateway,omitempty" yaml:"gateway,omitempty"`
	Routes          []string        `json:"routes,omitempty" yaml:"routes,omitempty"`
}

type DeviceClassParameters struct {
	Strategy     string     `json:"strategy" yaml:"strategy"`
	Profile      string     `json:"profile" yaml:"profile"`
	SlotsPerNode int        `json:"slotsPerNode,omitempty" yaml:"slotsPerNode,omitempty"`
	Paths        []PathSpec `json:"paths" yaml:"paths"`
}

type AddressSpec struct {
	Path string `json:"path" yaml:"path"`
	IP   string `json:"ip" yaml:"ip"`
}

type ClaimParameters struct {
	Addresses []AddressSpec `json:"addresses" yaml:"addresses"`
}

func Decode[T any](raw []byte) (T, error) {
	var value T
	if len(raw) == 0 {
		return value, fmt.Errorf("empty opaque parameters")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, fmt.Errorf("decode opaque parameters: %w", err)
	}
	return value, nil
}

func (p *DeviceClassParameters) NormalizeAndValidate() error {
	if p.Strategy == "" {
		return fmt.Errorf("strategy is required")
	}
	if p.Strategy != StrategyL2Announce && p.Strategy != StrategyIPReassign && p.Strategy != StrategyRouteRepoint {
		return fmt.Errorf("strategy %q is unsupported in anchor 0.2", p.Strategy)
	}
	if p.Profile == "" {
		return fmt.Errorf("profile is required")
	}
	if p.SlotsPerNode == 0 {
		p.SlotsPerNode = 1
	}
	if p.SlotsPerNode < 1 || p.SlotsPerNode > 14 {
		return fmt.Errorf("slotsPerNode must be between 1 and 14")
	}
	if len(p.Paths) == 0 {
		return fmt.Errorf("at least one path is required")
	}
	seen := map[string]bool{}
	seenInterfaces := map[string]bool{}
	seenTables := map[int]bool{}
	for i := range p.Paths {
		path := &p.Paths[i]
		if path.Name == "" || path.InterfaceName == "" {
			return fmt.Errorf("path %d requires name and interfaceName", i)
		}
		if len(path.InterfaceName) > 15 {
			return fmt.Errorf("path %q interfaceName %q exceeds the Linux 15-character limit", path.Name, path.InterfaceName)
		}
		if path.RoutingTable < 1 || path.RoutingTable > 252 {
			return fmt.Errorf("path %q routingTable must be between 1 and 252", path.Name)
		}
		if seen[path.Name] {
			return fmt.Errorf("duplicate path name %q", path.Name)
		}
		seen[path.Name] = true
		if seenInterfaces[path.InterfaceName] {
			return fmt.Errorf("duplicate interfaceName %q", path.InterfaceName)
		}
		seenInterfaces[path.InterfaceName] = true
		if seenTables[path.RoutingTable] {
			return fmt.Errorf("duplicate routingTable %d", path.RoutingTable)
		}
		seenTables[path.RoutingTable] = true
		if p.Strategy == StrategyL2Announce {
			if path.ParentInterface == "" || path.SubnetCIDR == "" {
				return fmt.Errorf("path %q requires parentInterface and subnetCidr for l2-announce", path.Name)
			}
			if path.SubnetID != "" || len(path.ENITagSelector) != 0 || len(path.RouteTableIDs) != 0 {
				return fmt.Errorf("path %q AWS subnet, ENI selector, and route-table fields are invalid for l2-announce", path.Name)
			}
			subnet, err := netip.ParsePrefix(path.SubnetCIDR)
			if err != nil || !subnet.Addr().Is4() || subnet != subnet.Masked() {
				return fmt.Errorf("path %q has invalid canonical IPv4 subnetCidr %q", path.Name, path.SubnetCIDR)
			}
		} else {
			if path.SubnetID == "" || len(path.ENITagSelector) == 0 {
				if p.Strategy != StrategyRouteRepoint || len(path.Subnets) == 0 || len(path.ENITagSelector) == 0 {
					return fmt.Errorf("path %q requires AWS subnet topology and eniTagSelector", path.Name)
				}
			}
			if path.ParentInterface != "" || path.SubnetCIDR != "" {
				return fmt.Errorf("path %q parentInterface and subnetCidr are only valid for l2-announce", path.Name)
			}
		}
		if p.Strategy == StrategyRouteRepoint {
			if path.SubnetID != "" || path.Gateway != "" {
				return fmt.Errorf("path %q route-repoint topology must use subnets instead of subnetId or gateway", path.Name)
			}
			if len(path.RouteTableIDs) == 0 {
				return fmt.Errorf("path %q requires routeTableIds for route-repoint", path.Name)
			}
			seenSubnets := map[string]bool{}
			for _, subnet := range path.Subnets {
				if subnet.SubnetID == "" || seenSubnets[subnet.SubnetID] {
					return fmt.Errorf("path %q has an empty or duplicate route-repoint subnet", path.Name)
				}
				seenSubnets[subnet.SubnetID] = true
				if (subnet.Gateway == "") != (len(path.Routes) == 0) {
					return fmt.Errorf("path %q subnet %q requires gateway when routes are configured", path.Name, subnet.SubnetID)
				}
				if subnet.Gateway != "" {
					gateway, err := netip.ParseAddr(subnet.Gateway)
					if err != nil || !gateway.Is4() {
						return fmt.Errorf("path %q subnet %q has invalid IPv4 gateway %q", path.Name, subnet.SubnetID, subnet.Gateway)
					}
				}
			}
		}
		if p.Strategy == StrategyIPReassign && len(path.RouteTableIDs) != 0 {
			return fmt.Errorf("path %q routeTableIds are only valid for route-repoint", path.Name)
		}
		if p.Strategy != StrategyRouteRepoint && len(path.Subnets) != 0 {
			return fmt.Errorf("path %q subnets are only valid for route-repoint", path.Name)
		}
		if p.Strategy != StrategyRouteRepoint && (path.Gateway == "") != (len(path.Routes) == 0) {
			return fmt.Errorf("path %q requires gateway and routes to be configured together", path.Name)
		}
		if path.Gateway == "" && p.Strategy != StrategyRouteRepoint {
			continue
		}
		if path.Gateway != "" {
			gateway, err := netip.ParseAddr(path.Gateway)
			if err != nil || !gateway.Is4() {
				return fmt.Errorf("path %q has invalid IPv4 gateway %q", path.Name, path.Gateway)
			}
		}
		seenRoutes := map[netip.Prefix]bool{}
		for _, value := range path.Routes {
			route, err := netip.ParsePrefix(value)
			if err != nil || !route.Addr().Is4() {
				return fmt.Errorf("path %q has invalid IPv4 route %q", path.Name, value)
			}
			if route != route.Masked() {
				return fmt.Errorf("path %q route %q must be a canonical network CIDR", path.Name, value)
			}
			if seenRoutes[route] {
				return fmt.Errorf("path %q has duplicate route %q", path.Name, value)
			}
			seenRoutes[route] = true
		}
	}
	return nil
}

func (p PathSpec) ResolveAWSSubnet(subnetID string) (AWSSubnetSpec, bool) {
	if len(p.Subnets) == 0 {
		return AWSSubnetSpec{SubnetID: p.SubnetID, Gateway: p.Gateway}, p.SubnetID == subnetID
	}
	for _, subnet := range p.Subnets {
		if subnet.SubnetID == subnetID {
			return subnet, true
		}
	}
	return AWSSubnetSpec{}, false
}

func (p ClaimParameters) Validate(class DeviceClassParameters, subnetCIDRs map[string]string) error {
	if len(p.Addresses) != len(class.Paths) {
		return fmt.Errorf("claim has %d addresses but profile %q requires %d", len(p.Addresses), class.Profile, len(class.Paths))
	}
	paths := map[string]PathSpec{}
	for _, path := range class.Paths {
		paths[path.Name] = path
	}
	seenPaths := map[string]bool{}
	seenIPs := map[netip.Addr]bool{}
	for _, address := range p.Addresses {
		path, ok := paths[address.Path]
		if !ok {
			return fmt.Errorf("address references unknown path %q", address.Path)
		}
		if seenPaths[address.Path] {
			return fmt.Errorf("duplicate address for path %q", address.Path)
		}
		seenPaths[address.Path] = true
		prefix, err := netip.ParsePrefix(address.IP)
		if err != nil || !prefix.Addr().Is4() {
			return fmt.Errorf("path %q has invalid IPv4 CIDR %q", address.Path, address.IP)
		}
		if seenIPs[prefix.Addr()] {
			return fmt.Errorf("duplicate IP %s", prefix.Addr())
		}
		seenIPs[prefix.Addr()] = true
		if class.Strategy == StrategyRouteRepoint && prefix.Bits() != 32 {
			return fmt.Errorf("route-repoint address %s for path %q must use a /32 prefix", prefix, address.Path)
		}
		cidr := subnetCIDRs[path.SubnetID]
		if class.Strategy == StrategyL2Announce {
			cidr = path.SubnetCIDR
		}
		if path.Gateway != "" && cidr == "" {
			return fmt.Errorf("cannot validate gateway for path %q without discovered subnet CIDR", address.Path)
		}
		if cidr != "" {
			subnet, err := netip.ParsePrefix(cidr)
			if err != nil {
				return fmt.Errorf("invalid discovered subnet CIDR %q: %w", cidr, err)
			}
			if class.Strategy == StrategyIPReassign || class.Strategy == StrategyL2Announce {
				if !subnet.Contains(prefix.Addr()) {
					return fmt.Errorf("address %s is outside subnet %s for path %q", prefix.Addr(), subnet, address.Path)
				}
				if prefix.Bits() != subnet.Bits() {
					return fmt.Errorf("address %s prefix length must match subnet %s for path %q", prefix, subnet, address.Path)
				}
				if class.Strategy == StrategyIPReassign && awsReservedIPv4(subnet, prefix.Addr()) {
					return fmt.Errorf("address %s is reserved by AWS in subnet %s", prefix.Addr(), subnet)
				}
			}
			if path.Gateway != "" {
				gateway, _ := netip.ParseAddr(path.Gateway)
				if !subnet.Contains(gateway) {
					return fmt.Errorf("gateway %s is outside subnet %s for path %q", gateway, subnet, address.Path)
				}
				if gateway == prefix.Addr() {
					return fmt.Errorf("gateway %s equals the claimed address for path %q", gateway, address.Path)
				}
			}
		}
	}
	return nil
}

func awsReservedIPv4(subnet netip.Prefix, address netip.Addr) bool {
	if !subnet.Addr().Is4() || subnet.Bits() > 30 {
		return false
	}
	base := subnet.Masked().Addr()
	reserved := base
	for i := 0; i < 4; i++ {
		if address == reserved {
			return true
		}
		reserved = reserved.Next()
	}
	last := base
	count := uint64(1) << uint64(32-subnet.Bits())
	for i := uint64(1); i < count; i++ {
		last = last.Next()
	}
	return address == last
}

func (p DeviceClassParameters) PathNames() []string {
	names := make([]string, 0, len(p.Paths))
	for _, path := range p.Paths {
		names = append(names, path.Name)
	}
	sort.Strings(names)
	return names
}

func TagsMatch(actual map[string]string, selector TagSelector) bool {
	for key, value := range selector {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func SafeName(parts ...string) string {
	name := strings.ToLower(strings.Join(parts, "-"))
	var out strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}
