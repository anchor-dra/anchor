//go:build linux

package node

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anchor-dra/anchor/internal/model"
)

type IPvlanInjector struct {
	Store *PlanStore
}

func (i *IPvlanInjector) Inject(ctx context.Context, plan PodNetworkPlan, namespace NetworkNamespace) error {
	return i.engine().Inject(ctx, plan, namespace)
}

func (i *IPvlanInjector) Verify(ctx context.Context, plan PodNetworkPlan, namespace NetworkNamespace) error {
	return i.engine().Verify(ctx, plan, namespace)
}

func (i *IPvlanInjector) Reconcile(ctx context.Context) ([]PlanReconcileResult, error) {
	return i.engine().Reconcile(ctx)
}

func (i *IPvlanInjector) engine() *PlanInjector {
	return &PlanInjector{Store: i.Store, Ops: linuxPathOps{}}
}

type linuxPathOps struct{}

func (linuxPathOps) InjectPath(ctx context.Context, plan PodNetworkPlan, path model.PlacementPath, namespace NetworkNamespace) (bool, error) {
	if namespace.Path == "" {
		return false, fmt.Errorf("pod network namespace path is empty")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validatePlacementPath(path); err != nil {
		return false, err
	}
	parent, err := verifiedHostParent(path)
	if err != nil {
		return false, err
	}
	alreadyPresent := false
	err = withNetNS(namespace.Path, func() error {
		link, err := netlink.LinkByName(path.InterfaceName)
		if err != nil {
			return nil
		}
		alreadyPresent = true
		return verifyPath(link, path)
	})
	if alreadyPresent && err == nil {
		return false, nil
	}
	if alreadyPresent {
		if deleteErr := (linuxPathOps{}).DeletePath(ctx, path, namespace); deleteErr != nil {
			return false, errors.Join(err, fmt.Errorf("remove drifted interface %q: %w", path.InterfaceName, deleteErr))
		}
	} else if err != nil {
		return false, err
	}

	temporaryName := temporaryLinkName(plan.ClaimUID, path.Name)
	if stale, lookupErr := netlink.LinkByName(temporaryName); lookupErr == nil {
		if err := netlink.LinkDel(stale); err != nil {
			return false, fmt.Errorf("delete stale temporary link %q: %w", temporaryName, err)
		}
	}
	child := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{Name: temporaryName, ParentIndex: parent.Attrs().Index},
		Mode:      netlink.IPVLAN_MODE_L2,
	}
	if err := netlink.LinkAdd(child); err != nil {
		return false, fmt.Errorf("create ipvlan child: %w", err)
	}
	cleanupHost := true
	defer func() {
		if cleanupHost {
			if link, lookupErr := netlink.LinkByName(temporaryName); lookupErr == nil {
				_ = netlink.LinkDel(link)
			}
		}
	}()
	target, err := netns.GetFromPath(namespace.Path)
	if err != nil {
		return false, fmt.Errorf("open pod network namespace: %w", err)
	}
	defer target.Close()
	if err := netlink.LinkSetNsFd(child, int(target)); err != nil {
		return false, fmt.Errorf("move ipvlan child into pod namespace: %w", err)
	}
	cleanupHost = false
	if err := withNetNS(namespace.Path, func() error {
		link, err := netlink.LinkByName(temporaryName)
		if err != nil {
			return fmt.Errorf("find moved ipvlan child: %w", err)
		}
		if err := netlink.LinkSetName(link, path.InterfaceName); err != nil {
			return fmt.Errorf("rename ipvlan child: %w", err)
		}
		link, err = netlink.LinkByName(path.InterfaceName)
		if err != nil {
			return err
		}
		if err := configurePath(link, path); err != nil {
			cleanupErr := deletePathPolicyRules(path)
			return errors.Join(err, cleanupErr, netlink.LinkDel(link))
		}
		if plan.Strategy == model.StrategyL2Announce {
			if err := announceIPv4(link, path.IP); err != nil {
				cleanupErr := deletePathPolicyRules(path)
				return errors.Join(fmt.Errorf("announce address %s on %s: %w", path.IP, path.InterfaceName, err), cleanupErr, netlink.LinkDel(link))
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		_ = (linuxPathOps{}).DeletePath(context.Background(), path, namespace)
		return false, err
	}
	return true, nil
}

func announceIPv4(link netlink.Link, cidr string) error {
	if link.Attrs().HardwareAddr == nil || len(link.Attrs().HardwareAddr) != 6 {
		return errors.New("interface has no Ethernet hardware address")
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("invalid IPv4 CIDR %q", cidr)
	}
	frame := gratuitousARPFrame(link.Attrs().HardwareAddr, prefix.Addr())
	protocol := htons(unix.ETH_P_ARP)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(protocol))
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	address := &unix.SockaddrLinklayer{Protocol: protocol, Ifindex: link.Attrs().Index, Halen: 6}
	copy(address.Addr[:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	return unix.Sendto(fd, frame, 0, address)
}

func gratuitousARPFrame(mac net.HardwareAddr, address netip.Addr) []byte {
	frame := make([]byte, 42)
	copy(frame[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], unix.ETH_P_ARP)
	binary.BigEndian.PutUint16(frame[14:16], 1)      // Ethernet
	binary.BigEndian.PutUint16(frame[16:18], 0x0800) // IPv4
	frame[18], frame[19] = 6, 4
	binary.BigEndian.PutUint16(frame[20:22], 1) // ARP request
	copy(frame[22:28], mac)
	ip := address.As4()
	copy(frame[28:32], ip[:])
	copy(frame[38:42], ip[:])
	return frame
}

func htons(value uint16) uint16 { return value<<8 | value>>8 }

func configurePath(link netlink.Link, path model.PlacementPath) error {
	address, err := netlink.ParseAddr(path.IP)
	if err != nil {
		return fmt.Errorf("parse address %q: %w", path.IP, err)
	}
	if err := netlink.AddrReplace(link, address); err != nil {
		return fmt.Errorf("assign address %s: %w", path.IP, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring interface up: %w", err)
	}

	connected, _ := netlink.ParseIPNet(path.SubnetCIDR)
	if connected != nil {
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: connected, Scope: netlink.SCOPE_LINK, Table: path.RoutingTable}); err != nil {
			return fmt.Errorf("configure connected route in table %d: %w", path.RoutingTable, err)
		}
	}
	var gateway net.IP
	if path.Gateway != "" {
		gateway = net.ParseIP(path.Gateway)
		gatewayRoute := &net.IPNet{IP: gateway, Mask: net.CIDRMask(32, 32)}
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: gatewayRoute, Scope: netlink.SCOPE_LINK, Table: path.RoutingTable}); err != nil {
			return fmt.Errorf("configure on-link gateway: %w", err)
		}
	}
	for _, destination := range path.Routes {
		dst, err := netlink.ParseIPNet(destination)
		if err != nil {
			return fmt.Errorf("parse route %q: %w", destination, err)
		}
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Gw: gateway, Table: path.RoutingTable}); err != nil {
			return fmt.Errorf("configure route %s: %w", destination, err)
		}
	}
	for _, rule := range pathPolicyRules(path) {
		if err := replaceRule(rule); err != nil {
			return fmt.Errorf("configure policy rule: %w", err)
		}
	}
	return nil
}

func replaceRule(desired *netlink.Rule) error {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	found := false
	for _, rule := range rules {
		if samePolicyRule(rule, *desired) {
			found = true
			continue
		}
		if conflictingPolicyRule(rule, *desired) {
			return fmt.Errorf("existing policy rule %s conflicts with %s", rule.String(), desired)
		}
	}
	if found {
		return nil
	}
	return netlink.RuleAdd(desired)
}

func verifyPath(link netlink.Link, path model.PlacementPath) error {
	if _, ok := link.(*netlink.IPVlan); !ok {
		return fmt.Errorf("interface %q is not ipvlan", path.InterfaceName)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		return fmt.Errorf("interface %q is down", path.InterfaceName)
	}
	wanted, _ := netlink.ParseAddr(path.IP)
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	found := false
	for _, address := range addresses {
		if address.Equal(*wanted) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("interface %q is missing address %s", path.InterfaceName, path.IP)
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Table:     path.RoutingTable,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
	if err != nil {
		return fmt.Errorf("list routes for interface %q table %d: %w", path.InterfaceName, path.RoutingTable, err)
	}
	connected, _ := netlink.ParseIPNet(path.SubnetCIDR)
	if connected != nil && !hasRoute(routes, connected, nil) {
		return fmt.Errorf("interface %q is missing connected route %s in table %d", path.InterfaceName, path.SubnetCIDR, path.RoutingTable)
	}
	var gateway net.IP
	if path.Gateway != "" {
		gateway = net.ParseIP(path.Gateway)
		gatewayRoute := &net.IPNet{IP: gateway, Mask: net.CIDRMask(32, 32)}
		if !hasRoute(routes, gatewayRoute, nil) {
			return fmt.Errorf("interface %q is missing on-link gateway route %s in table %d", path.InterfaceName, path.Gateway, path.RoutingTable)
		}
	}
	for _, destination := range path.Routes {
		dst, _ := netlink.ParseIPNet(destination)
		if !hasRoute(routes, dst, gateway) {
			return fmt.Errorf("interface %q is missing route %s via %s in table %d", path.InterfaceName, destination, path.Gateway, path.RoutingTable)
		}
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, desired := range pathPolicyRules(path) {
		found := false
		for _, actual := range rules {
			if samePolicyRule(actual, *desired) {
				found = true
				continue
			}
			if conflictingPolicyRule(actual, *desired) {
				return fmt.Errorf("interface %q has a conflicting policy rule %s", path.InterfaceName, actual.String())
			}
		}
		if !found {
			return fmt.Errorf("interface %q is missing policy rule %s", path.InterfaceName, desired)
		}
	}
	return nil
}

func hasRoute(routes []netlink.Route, destination *net.IPNet, gateway net.IP) bool {
	for _, route := range routes {
		actualDestination := route.Dst
		if actualDestination == nil {
			actualDestination = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
		}
		if !ipNetEqual(actualDestination, destination) {
			continue
		}
		if gateway == nil && route.Gw == nil {
			return true
		}
		if gateway != nil && route.Gw.Equal(gateway) {
			return true
		}
	}
	return false
}

func (linuxPathOps) VerifyPath(ctx context.Context, path model.PlacementPath, namespace NetworkNamespace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := verifiedHostParent(path); err != nil {
		return err
	}
	return withNetNS(namespace.Path, func() error {
		link, err := netlink.LinkByName(path.InterfaceName)
		if err != nil {
			return fmt.Errorf("find interface %q: %w", path.InterfaceName, err)
		}
		return verifyPath(link, path)
	})
}

func verifiedHostParent(path model.PlacementPath) (netlink.Link, error) {
	parent, err := netlink.LinkByName(path.Interface)
	if err != nil {
		return nil, fmt.Errorf("find host parent %q: %w", path.Interface, err)
	}
	actualMAC := ""
	if parent.Attrs().HardwareAddr != nil {
		actualMAC = strings.ToLower(parent.Attrs().HardwareAddr.String())
	}
	if path.ParentMAC == "" || actualMAC != strings.ToLower(path.ParentMAC) {
		return nil, fmt.Errorf("host parent %q MAC %q does not match approved MAC %q", path.Interface, actualMAC, path.ParentMAC)
	}
	if parent.Attrs().Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("host parent %q is down", path.Interface)
	}
	return parent, nil
}

func (linuxPathOps) DeletePath(ctx context.Context, path model.PlacementPath, namespace NetworkNamespace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return withNetNS(namespace.Path, func() error {
		// Policy rules survive LinkDel. Remove only rules belonging to this plan,
		// even if the interface was already removed during failed injection.
		if err := deletePathPolicyRules(path); err != nil {
			return err
		}
		link, err := netlink.LinkByName(path.InterfaceName)
		if err != nil {
			var missing netlink.LinkNotFoundError
			if errors.As(err, &missing) {
				return nil
			}
			return err
		}
		return netlink.LinkDel(link)
	})
}

func validatePlacementPath(path model.PlacementPath) error {
	if path.InterfaceName == "" || len(path.InterfaceName) > 15 || path.Interface == "" || path.ParentMAC == "" {
		return fmt.Errorf("path %q has incomplete interface identity", path.Name)
	}
	if path.RoutingTable < 1 || path.RoutingTable > 252 {
		return fmt.Errorf("path %q has invalid routing table %d", path.Name, path.RoutingTable)
	}
	if prefix, err := netip.ParsePrefix(path.IP); err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("path %q has invalid IPv4 address %q", path.Name, path.IP)
	}
	return model.ValidatePreferredDestinations([]model.PathSpec{{Name: path.Name, Routes: path.Routes, PreferredDestinations: path.PreferredDestinations}})
}

func temporaryLinkName(claimUID, path string) string {
	hash := sha256.Sum256([]byte(claimUID + "/" + path))
	return fmt.Sprintf("anc%x", hash[:5])
}

func withNetNS(path string, fn func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		unlock := true
		defer func() {
			if unlock {
				runtime.UnlockOSThread()
			}
		}()
		original, err := netns.Get()
		if err != nil {
			result <- fmt.Errorf("open current network namespace: %w", err)
			return
		}
		defer original.Close()
		target, err := netns.GetFromPath(path)
		if err != nil {
			result <- fmt.Errorf("open network namespace %q: %w", path, err)
			return
		}
		defer target.Close()
		if err := netns.Set(target); err != nil {
			result <- fmt.Errorf("enter network namespace %q: %w", path, err)
			return
		}
		operationErr := fn()
		if err := netns.Set(original); err != nil {
			// This dedicated goroutine exits while still locked, which causes the
			// runtime to retire the poisoned OS thread instead of reusing it.
			unlock = false
			result <- errors.Join(operationErr, fmt.Errorf("restore original network namespace: %w", err))
			return
		}
		result <- operationErr
	}()
	return <-result
}

func ipNetEqual(left, right *net.IPNet) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.IP.Equal(right.IP) && left.Mask.String() == right.Mask.String()
}

// Source rules always win; destination preferences only guide an initial lookup
// without a matching carrier source. Both ranges precede Linux's main table.
func pathPolicyRules(path model.PlacementPath) []*netlink.Rule {
	prefix := netip.MustParsePrefix(path.IP)
	source := netlink.NewRule()
	source.Family = netlink.FAMILY_V4
	source.Src = &net.IPNet{IP: net.IP(prefix.Addr().AsSlice()), Mask: net.CIDRMask(32, 32)}
	source.Priority = 10000 + path.RoutingTable
	source.Table = path.RoutingTable
	rules := []*netlink.Rule{source}
	for _, value := range path.PreferredDestinations {
		rule := netlink.NewRule()
		rule.Family = netlink.FAMILY_V4
		rule.Dst, _ = netlink.ParseIPNet(value)
		rule.Priority = 20000 + path.RoutingTable
		rule.Table = path.RoutingTable
		rules = append(rules, rule)
	}
	return rules
}

func samePolicyRule(actual, desired netlink.Rule) bool {
	if !ipNetEqual(actual.Src, desired.Src) || !ipNetEqual(actual.Dst, desired.Dst) {
		return false
	}
	actual.Src, actual.Dst, desired.Src, desired.Dst = nil, nil, nil, nil
	// Family was implicit in older source-rule writes. Kernel dumps set AF_INET.
	if desired.Family == 0 {
		desired.Family = netlink.FAMILY_V4
	}
	return reflect.DeepEqual(actual, desired)
}

func conflictingPolicyRule(actual, desired netlink.Rule) bool {
	// Equal-priority overlapping destinations must not silently shadow the
	// requested lookup. Rules with other selectors (e.g. marks) remain separate.
	overlaps := actual.Dst == nil || desired.Dst == nil || actual.Dst.Contains(desired.Dst.IP) || desired.Dst.Contains(actual.Dst.IP)
	actual.Table, actual.Dst = desired.Table, desired.Dst
	return overlaps && samePolicyRule(actual, desired)
}

func deletePathPolicyRules(path model.PlacementPath) error {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, actual := range rules {
		for _, desired := range pathPolicyRules(path) {
			if samePolicyRule(actual, *desired) {
				if err := netlink.RuleDel(&actual); err != nil && !errors.Is(err, unix.ENOENT) {
					return err
				}
				break
			}
		}
	}
	return nil
}
