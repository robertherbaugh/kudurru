package rpz

import (
	"net/netip"
	"strings"
	"testing"
)

func TestAddressEncodingBounds(t *testing.T) {
	for _, name := range []string{
		"+24.0.2.0.192", "24.0.+2.0.192", "0.0.0.0.0", "33.0.0.0.0", "24.1.2.0.192", "24.0.256.0.192",
		"64.zz.zz.2001", "129.zz.2001", "64.0.0.0.0.0.0.0.zz.2001", "64.0.0.0.0.0.0.0.0.2001", "64.zz.12345", "64.zz.+1", "64.1.zz.2001",
	} {
		if _, err := addressPrefix(strings.Split(name, ".")); err == nil {
			t.Errorf("accepted malformed prefix %q", name)
		}
	}
	for name, want := range map[string]string{
		"24.0.2.0.192":            "192.0.2.0/24",
		"64.0.0.0.0.0.0.db8.2001": "2001:db8::/64",
		"64.zz.db8.2001":          "2001:db8::/64",
		"128.1.zz.db8.2001":       "2001:db8::1/128",
	} {
		got, err := addressPrefix(strings.Split(name, "."))
		if err != nil || got != netip.MustParsePrefix(want) {
			t.Errorf("%s = %v, %v", name, got, err)
		}
	}
	z := fixtureZone(t, "24.0.2.0.192.rpz-ip.policy.test. 60 IN CNAME .")
	d := NewSelection(z).Evaluate(Context{Name: "unrelated.test.", Addresses: []netip.Addr{netip.MustParseAddr("::ffff:192.0.2.7")}}, testNow)
	if d.Action != ActionNXDOMAIN {
		t.Fatal("mapped IPv4 response bypassed IPv4 policy")
	}
}
