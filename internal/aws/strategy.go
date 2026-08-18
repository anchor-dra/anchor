package aws

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/anchor-dra/anchor/internal/model"
)

type Endpoint struct {
	ClaimUID    string
	ClaimName   string
	NodeName    string
	Path        model.PlacementPath
	PreviousENI string
	ForceSteal  bool
}

type PlacementStrategy interface {
	Place(context.Context, Endpoint) error
	Release(context.Context, Endpoint) error
	Validate(context.Context, Endpoint) error
}

func AddressOnly(cidr string) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() {
		return "", fmt.Errorf("invalid IPv4 CIDR %q", cidr)
	}
	return prefix.Addr().String(), nil
}
