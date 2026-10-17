//go:build !linux

package dhcp

import (
	"context"
	"errors"
	"net/netip"
)

func probeAddress(_ context.Context, _ Config, _ netip.Addr) (bool, error) {
	return false, errors.New("dhcp: conflict detection requires Linux")
}
