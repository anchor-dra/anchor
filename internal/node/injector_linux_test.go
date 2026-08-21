//go:build linux

package node

import (
	"bytes"
	"net"
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/anchor-dra/anchor/internal/model"
)

func TestLinuxPlacementPathValidation(t *testing.T) {
	valid := model.PlacementPath{
		Name: "a", InterfaceName: "carrier-a", Interface: "ens6",
		ParentMAC: "02:00:00:00:00:01", RoutingTable: 101, IP: "10.0.1.10/24",
	}
	if err := validatePlacementPath(valid); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	invalid := valid
	invalid.RoutingTable = 253
	if err := validatePlacementPath(invalid); err == nil {
		t.Fatal("reserved routing table was accepted")
	}
	invalid = valid
	invalid.InterfaceName = "interface-name-is-too-long"
	if err := validatePlacementPath(invalid); err == nil {
		t.Fatal("oversized Linux interface name was accepted")
	}
}

func TestGratuitousARPFrameAnnouncesTheClaimedAddress(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	frame := gratuitousARPFrame(mac, netip.MustParseAddr("10.50.1.10"))
	if len(frame) != 42 || !bytes.Equal(frame[6:12], mac) {
		t.Fatalf("invalid Ethernet header: %x", frame)
	}
	if got := net.IP(frame[28:32]).String(); got != "10.50.1.10" {
		t.Fatalf("unexpected sender address %s", got)
	}
	if got := net.IP(frame[38:42]).String(); got != "10.50.1.10" {
		t.Fatalf("unexpected gratuitous target address %s", got)
	}
}

func TestLinuxRouteVerificationMatchesDestinationAndGateway(t *testing.T) {
	destination := &net.IPNet{IP: net.ParseIP("192.0.2.0"), Mask: net.CIDRMask(24, 32)}
	gateway := net.ParseIP("10.0.1.1")
	routes := []netlink.Route{{Dst: destination, Gw: gateway}}
	if !hasRoute(routes, destination, gateway) {
		t.Fatal("expected route was not matched")
	}
	if hasRoute(routes, destination, net.ParseIP("10.0.1.2")) {
		t.Fatal("route with the wrong gateway was matched")
	}
}

func TestTemporaryLinkNameIsStableAndLinuxSafe(t *testing.T) {
	first := temporaryLinkName("claim-uid", "path-a")
	if first != temporaryLinkName("claim-uid", "path-a") {
		t.Fatal("temporary interface name is not stable")
	}
	if len(first) > 15 || first == temporaryLinkName("claim-uid", "path-b") {
		t.Fatalf("unsafe or colliding temporary interface names: %q", first)
	}
}
