package dhcpsetup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestSetupUsesRuntimeTopologyValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]config.DHCPScope)
		want string
	}{
		{name: "valid shared transport"},
		{name: "ambiguous relay", edit: func(scopes []config.DHCPScope) {
			scopes[0].TrustedRelays = append([]config.DHCPTrustedRelay(nil), scopes[1].TrustedRelays...)
		}, want: "ambiguous trusted relay"},
		{name: "foreign server outside pool", edit: func(scopes []config.DHCPScope) {
			scopes[1].Interface, scopes[1].ServerIP = "other0", "192.0.2.200"
		}, want: "another server address"},
		{name: "foreign gateway at mutable router", edit: func(scopes []config.DHCPScope) {
			scopes[1].TrustedRelays[0].GatewayIP = scopes[0].Router
		}, want: "could allocate relay gateway"},
		{name: "foreign gateway outside pool", edit: func(scopes []config.DHCPScope) {
			scopes[1].TrustedRelays[0].GatewayIP = "192.0.2.200"
		}, want: "could allocate relay gateway"},
		{name: "foreign gateway at immutable server", edit: func(scopes []config.DHCPScope) {
			scopes[1].TrustedRelays[0].GatewayIP = scopes[0].ServerIP
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scopes := []config.DHCPScope{
				{Name: "default", Management: "api", Interface: "fixture0", ServerIP: "192.0.2.2", Subnet: "192.0.2.0/24",
					RangeStart: "192.0.2.10", RangeEnd: "192.0.2.20", Router: "192.0.2.1", DNSServers: []string{"192.0.2.2"}},
				{Name: "remote", Management: "api", Interface: "fixture0", ServerIP: "192.0.2.2", Subnet: "203.0.113.0/24", RelayOnly: true,
					RangeStart: "203.0.113.10", RangeEnd: "203.0.113.20", Router: "203.0.113.1", DNSServers: []string{"192.0.2.2"},
					TrustedRelays: []config.DHCPTrustedRelay{{SourceIP: "198.51.100.1", GatewayIP: "203.0.113.1"}}},
			}
			if tc.edit != nil {
				tc.edit(scopes)
			}
			for _, enabled := range []bool{false, true} {
				_, err := validateScopes(context.Background(), enabled, scopes, true)
				if tc.want == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrInvalidConfiguration) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("enabled=%v: validation error = %v, want invalid configuration containing %q", enabled, err, tc.want)
				}
			}
		})
	}
}
