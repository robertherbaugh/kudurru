package zone

import (
	"net/netip"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestNotifyOptionsFreezeAndFailClosed(t *testing.T) {
	config := dnsZone("example.org.")
	config.Spec.Policy = public.ZonePolicy{Notify: public.NotifySpecified, NotifyTargets: []string{"192.0.2.1"}, TransferTSIGKeyRefs: []string{"transfer."}, NotifyTSIGKeyRef: "NOTIFY", NotifySource: "127.0.0.2"}
	model, errs := Build(config, nil, BuildOptions{})
	if model == nil || !errs.Empty() {
		t.Fatal("valid policy did not build")
	}
	config.Spec.Policy.NotifyTSIGKeyRef, config.Spec.Policy.NotifySource = "successor.", "127.0.0.3"
	if model.NotifyKey() != "notify." || model.NotifySource() != netip.MustParseAddr("127.0.0.2") {
		t.Fatal("zone model did not retain its frozen NOTIFY choices")
	}
	for _, tc := range []struct{ field, key, source string }{
		{"notifyTSIGKeyRef", "a..b", ""},
		{"notifySource", "", "hostname"},
		{"notifySource", "", "127.0.0.1:53"},
		{"notifySource", "", "fe80::1%lo"},
		{"notifySource", "", "0.0.0.0"},
		{"notifySource", "", "::"},
		{"notifySource", "", "::ffff:127.0.0.1"},
		{"notifySource", "", "224.0.0.1"},
	} {
		t.Run(tc.field+tc.key+tc.source, func(t *testing.T) {
			config := dnsZone("example.org.")
			config.Spec.Policy = public.ZonePolicy{Notify: public.NotifySpecified, NotifyTargets: []string{"192.0.2.1"}, TransferTSIGKeyRefs: []string{"transfer."}, NotifyTSIGKeyRef: tc.key, NotifySource: tc.source}
			model, errs := Build(config, nil, BuildOptions{})
			if model == nil || len(errs.Zone) != 0 || len(errs.Policy) != 1 || errs.Policy[0].Field != "spec.policy."+tc.field || model.NotifyMode() != public.NotifyNone {
				t.Fatal("invalid explicit NOTIFY option did not fail closed with a policy diagnosis")
			}
			if _, err := BuildCommittedUnavailable(config); err == nil {
				t.Fatal("invalid NOTIFY option entered committed primary configuration")
			}
		})
	}
}

func TestNotifyOptionsKeyNameBounds(t *testing.T) {
	maximum := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for _, ref := range []string{maximum, maximum + "."} {
		if got, err := ParseNotifyKeyRef(ref); err != nil || got != maximum+"." {
			t.Fatal("maximum DNS key name was not accepted and canonicalized")
		}
	}
	for _, ref := range []string{maximum + "d", maximum + "d.", strings.Repeat("a", 64), "", "."} {
		if _, err := ParseNotifyKeyRef(ref); err == nil {
			t.Fatal("invalid explicit DNS key name was accepted")
		}
	}
}
