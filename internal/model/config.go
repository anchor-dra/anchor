package model

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const StrategyIPReassign = "ip-reassign"

type TagSelector map[string]string

type PathSpec struct {
	Name           string      `json:"name" yaml:"name"`
	SubnetID       string      `json:"subnetId" yaml:"subnetId"`
	ENITagSelector TagSelector `json:"eniTagSelector" yaml:"eniTagSelector"`
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
		p.Strategy = StrategyIPReassign
	}
	if p.Strategy != StrategyIPReassign {
		return fmt.Errorf("strategy %q is unsupported in anchor 0.1", p.Strategy)
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
	for i := range p.Paths {
		path := &p.Paths[i]
		if path.Name == "" || path.SubnetID == "" {
			return fmt.Errorf("path %d requires name and subnetId", i)
		}
		if seen[path.Name] {
			return fmt.Errorf("duplicate path name %q", path.Name)
		}
		seen[path.Name] = true
		if len(path.ENITagSelector) == 0 {
			return fmt.Errorf("path %q requires eniTagSelector", path.Name)
		}
	}
	return nil
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
		if cidr := subnetCIDRs[path.SubnetID]; cidr != "" {
			subnet, err := netip.ParsePrefix(cidr)
			if err != nil {
				return fmt.Errorf("invalid discovered subnet CIDR %q: %w", cidr, err)
			}
			if !subnet.Contains(prefix.Addr()) {
				return fmt.Errorf("address %s is outside subnet %s for path %q", prefix.Addr(), subnet, address.Path)
			}
			if prefix.Bits() != subnet.Bits() {
				return fmt.Errorf("address %s prefix length must match subnet %s for path %q", prefix, subnet, address.Path)
			}
			if awsReservedIPv4(subnet, prefix.Addr()) {
				return fmt.Errorf("address %s is reserved by AWS in subnet %s", prefix.Addr(), subnet)
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
