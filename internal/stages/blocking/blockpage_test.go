package blocking

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

// TestBlockPageResponse: blockPage answers A and AAAA with the block page
// addresses, NODATA for other types and for a family without an address,
// EDE 15 naming the list; without any address (block page off) it answers
// like zeroIP. A BlockList's blockPage response wins over the policy's mode.
func TestBlockPageResponse(t *testing.T) {
	ix := fixture(t)
	page := Options{DefaultResponse: "blockPage", BlockPageV4: netip.MustParseAddr("192.0.2.80"), BlockPageV6: netip.MustParseAddr("2001:db8::80")}
	c := newChain(t, ix, page)
	for _, tc := range []struct {
		qtype uint16
		want  []string
	}{
		{dns.TypeA, []string{"x.ads.example.\t600\tIN\tA\t192.0.2.80"}},
		{dns.TypeAAAA, []string{"x.ads.example.\t600\tIN\tAAAA\t2001:db8::80"}},
		{dns.TypeHTTPS, nil},
		{dns.TypeMX, nil},
	} {
		resp, err := c.ask(t, "10.0.0.5", "x.ads.example.", tc.qtype, true)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.Rcode != dns.RcodeSuccess || !slices.Equal(answers(resp.Msg), tc.want) {
			t.Errorf("%s: %s %v, want %v", dns.TypeToString[tc.qtype], dns.RcodeToString[resp.Msg.Rcode], answers(resp.Msg), tc.want)
		}
		if code, text, ok := ede(resp.Msg); !ok || code != dns.ExtendedErrorCodeBlocked || text != "default/ads" {
			t.Errorf("%s: EDE %d %q %v", dns.TypeToString[tc.qtype], code, text, ok)
		}
		if resp.Status != pipeline.StatusBlockedList || resp.List != "default/ads" {
			t.Errorf("%s: status %s list %q", dns.TypeToString[tc.qtype], resp.Status, resp.List)
		}
	}

	// IPv4 only: AAAA is NODATA.
	c4 := newChain(t, ix, Options{DefaultResponse: "blockPage", BlockPageV4: netip.MustParseAddr("192.0.2.80")})
	if resp, _ := c4.ask(t, "10.0.0.5", "x.ads.example.", dns.TypeAAAA, false); resp.Msg.Rcode != dns.RcodeSuccess || len(resp.Msg.Answer) != 0 {
		t.Errorf("AAAA without a v6 block page address: %v", resp.Msg)
	}
	// No address at all: zeroIP.
	off := newChain(t, ix, Options{DefaultResponse: "blockPage"})
	if resp, _ := off.ask(t, "10.0.0.5", "x.ads.example.", dns.TypeA, false); !slices.Equal(answers(resp.Msg), []string{"x.ads.example.\t600\tIN\tA\t0.0.0.0"}) {
		t.Errorf("blockPage without addresses: %v", answers(resp.Msg))
	}
	// Reconfigure switches the addresses at runtime.
	c.stage.Reconfigure(Options{DefaultResponse: "blockPage", BlockPageV4: netip.MustParseAddr("192.0.2.81")})
	if resp, _ := c.ask(t, "10.0.0.5", "x.ads.example.", dns.TypeA, false); !slices.Equal(answers(resp.Msg), []string{"x.ads.example.\t600\tIN\tA\t192.0.2.81"}) {
		t.Errorf("after Reconfigure: %v", answers(resp.Msg))
	}

	// The list's response beats the policy's blockMode (nxdomain here).
	lists := compileLists(t, map[string][]string{"default/ads": {"||ads.example^"}}, nil)
	ads := bl("ads", "block", urlSource)
	ads.Spec.Response = "blockPage"
	ix2 := policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{cg("default", v1alpha1.ClientMatcher{CIDR: "10.0.0.0/8"})},
		Policies:     []*v1alpha1.Policy{{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads"}, BlockMode: "nxdomain"}}},
		BlockLists:   []*v1alpha1.BlockList{ads},
		Lists:        lists,
	}, policy.Options{})
	c5 := newChain(t, ix2, page)
	if resp, _ := c5.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false); !slices.Equal(answers(resp.Msg), []string{"ads.example.\t600\tIN\tA\t192.0.2.80"}) {
		t.Errorf("list blockPage response: %s %v", dns.RcodeToString[resp.Msg.Rcode], answers(resp.Msg))
	}
	// The policy's blockPage mode applies to lists without a response.
	ads.Spec.Response = ""
	ix3 := policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{cg("default", v1alpha1.ClientMatcher{CIDR: "10.0.0.0/8"})},
		Policies:     []*v1alpha1.Policy{{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads"}, BlockMode: "blockPage"}}},
		BlockLists:   []*v1alpha1.BlockList{ads},
		Lists:        lists,
	}, policy.Options{})
	c6 := newChain(t, ix3, Options{DefaultResponse: "nxdomain", BlockPageV4: netip.MustParseAddr("192.0.2.80")})
	if resp, _ := c6.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false); !slices.Equal(answers(resp.Msg), []string{"ads.example.\t600\tIN\tA\t192.0.2.80"}) {
		t.Errorf("policy blockPage mode: %s %v", dns.RcodeToString[resp.Msg.Rcode], answers(resp.Msg))
	}
}

func TestOptionsFromConfigBlockPage(t *testing.T) {
	cfg := &config.Config{
		Blocking:  config.Blocking{Enabled: true, DefaultResponse: "blockPage"},
		BlockPage: config.BlockPage{Enabled: true, IPv4: "192.0.2.80"},
	}
	o := OptionsFromConfig(cfg)
	if o.BlockPageV4 != netip.MustParseAddr("192.0.2.80") || o.BlockPageV6.IsValid() || o.DefaultResponse != "blockPage" {
		t.Errorf("OptionsFromConfig: %+v", o)
	}
	cfg.BlockPage.Enabled = false
	if o := OptionsFromConfig(cfg); o.BlockPageV4.IsValid() {
		t.Errorf("block page off must not answer its address: %+v", o)
	}
}
