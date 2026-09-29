//go:build linux

package node

import (
	"context"
	"net"
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/anchor-dra/anchor/internal/model"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Run in a disposable privileged Linux container with ANCHOR_NETNS_TEST=1.
// This exercises real kernel rule round-trips, route/source selection, drift,
// idempotency, and cleanup when the interface has already disappeared.
func TestPolicyRulesKernelLifecycle(t *testing.T) {
	if os.Getenv("ANCHOR_NETNS_TEST") != "1" {
		t.Skip("requires an isolated privileged Linux runner")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	target, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	defer func() {
		if err := netns.Set(original); err != nil {
			t.Errorf("restore namespace: %v", err)
		}
	}()
	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "parent"}}
	if err := netlink.LinkAdd(parent); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(parent); err != nil {
		t.Fatal(err)
	}
	child := &netlink.IPVlan{LinkAttrs: netlink.LinkAttrs{Name: "net1", ParentIndex: parent.Attrs().Index}, Mode: netlink.IPVLAN_MODE_L2}
	if err := netlink.LinkAdd(child); err != nil {
		t.Fatal(err)
	}
	path := model.PlacementPath{Name: "a", InterfaceName: "net1", RoutingTable: 101, IP: "10.200.0.40/32", SubnetCIDR: "10.60.80.0/26", Gateway: "10.60.80.1", Routes: []string{"10.201.0.0/24", "10.201.1.0/24"}, PreferredDestinations: []string{"10.201.0.0/25", "10.201.0.128/25"}}
	for i := 0; i < 2; i++ {
		if err := configurePath(child, path); err != nil {
			t.Fatal(err)
		}
	}
	refreshed, err := netlink.LinkByName("net1")
	if err != nil {
		t.Fatal(err)
	}
	child = refreshed.(*netlink.IPVlan)
	if err := verifyPath(child, path); err != nil {
		t.Fatal(err)
	}
	parentB := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "parentB"}}
	if err := netlink.LinkAdd(parentB); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(parentB); err != nil {
		t.Fatal(err)
	}
	childB := &netlink.IPVlan{LinkAttrs: netlink.LinkAttrs{Name: "net2", ParentIndex: parentB.Attrs().Index}, Mode: netlink.IPVLAN_MODE_L2}
	if err := netlink.LinkAdd(childB); err != nil {
		t.Fatal(err)
	}
	pathB := path
	pathB.Name, pathB.InterfaceName, pathB.RoutingTable = "b", "net2", 102
	pathB.IP, pathB.SubnetCIDR, pathB.Gateway = "10.200.1.40/32", "10.60.81.0/26", "10.60.81.1"
	pathB.PreferredDestinations = []string{"10.201.1.0/24"}
	if err := configurePath(childB, pathB); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dst, src, want string
		index          int
	}{
		{"10.201.0.40", "", "10.200.0.40", child.Attrs().Index},
		{"10.201.1.40", "", "10.200.1.40", childB.Attrs().Index},
		{"10.201.1.40", "10.200.0.40", "10.200.0.40", child.Attrs().Index},
		{"10.201.0.40", "10.200.1.40", "10.200.1.40", childB.Attrs().Index},
	} {
		routes, err := netlink.RouteGetWithOptions(net.ParseIP(tc.dst), &netlink.RouteGetOptions{SrcAddr: net.ParseIP(tc.src)})
		if err != nil || len(routes) != 1 || (tc.src == "" && routes[0].Src.String() != tc.want) || routes[0].LinkIndex != tc.index {
			t.Fatalf("route %s from %s: %+v %v", tc.dst, tc.src, routes, err)
		}
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range rules {
		if r.Priority == 10101 || r.Priority == 20101 {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("expected one source and two destination rules, got %d: %+v", count, rules)
	}
	conflict := *pathPolicyRules(path)[1]
	conflict.Table = 222
	conflict.Dst, _ = netlink.ParseIPNet("10.201.0.0/24")
	if err := netlink.RuleAdd(&conflict); err != nil {
		t.Fatal(err)
	}
	if err := verifyPath(child, path); err == nil {
		t.Fatal("overlapping conflicting rule was not detected")
	}
	if err := configurePath(child, path); err == nil {
		t.Fatal("conflicting rule was silently accepted during configuration")
	}
	if err := netlink.RuleDel(&conflict); err != nil {
		t.Fatal(err)
	}
	// A restricted rule must not satisfy verification or be removed as ours.
	foreign := *pathPolicyRules(path)[1]
	foreign.Mark = 17
	if err := netlink.RuleAdd(&foreign); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RuleDel(pathPolicyRules(path)[2]); err != nil {
		t.Fatal(err)
	}
	if err := verifyPath(child, path); err == nil {
		t.Fatal("missing second destination rule was not detected")
	}
	if err := configurePath(child, path); err != nil {
		t.Fatal(err)
	}
	if err := verifyPath(child, path); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkDel(child); err != nil {
		t.Fatal(err)
	}
	// DeletePath must remove rules even after the interface disappeared.
	ns := NetworkNamespace{Path: "/proc/self/task/" + strconv.Itoa(unix.Gettid()) + "/ns/net"}
	if err := (linuxPathOps{}).DeletePath(context.Background(), path, ns); err != nil {
		t.Fatal(err)
	}
	if err := deletePathPolicyRules(path); err != nil {
		t.Fatal(err)
	}
	rules, err = netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if (r.Priority == 10101 || r.Priority == 20101) && r.Mark != 17 {
			t.Fatalf("owned policy rule leaked: %+v", r)
		}
	}
	found := false
	for _, r := range rules {
		if r.Mark == 17 {
			found = true
		}
	}
	if !found {
		t.Fatal("cleanup deleted a foreign marked rule")
	}
	// A /24 address supplies its normal connected main route without preferences.
	connected := &netlink.IPVlan{LinkAttrs: netlink.LinkAttrs{Name: "connected", ParentIndex: parent.Attrs().Index}, Mode: netlink.IPVLAN_MODE_L2}
	if err := netlink.LinkAdd(connected); err != nil {
		t.Fatal(err)
	}
	sameSubnet := model.PlacementPath{Name: "connected", InterfaceName: "connected", RoutingTable: 103, IP: "10.50.1.10/24", SubnetCIDR: "10.50.1.0/24"}
	if err := configurePath(connected, sameSubnet); err != nil {
		t.Fatal(err)
	}
	routes, err := netlink.RouteGet(net.ParseIP("10.50.1.20"))
	if err != nil || len(routes) != 1 || routes[0].Src.String() != "10.50.1.10" || routes[0].LinkIndex != connected.Attrs().Index {
		t.Fatalf("same-subnet route changed: %+v %v", routes, err)
	}
}
