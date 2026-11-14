package network_test

import (
	"context"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func separateNotifyKeys(t *testing.T, notifySecret string) *dnsserver.StaticKeyring {
	t.Helper()
	keys := []dnsserver.TSIGKey{{Name: "key.", Algorithm: dns.HmacSHA256, Secret: []byte("test-original-secret")}}
	if notifySecret != "" {
		keys = append(keys, dnsserver.TSIGKey{Name: "notify.", Algorithm: dns.HmacSHA256, Secret: []byte(notifySecret)})
	}
	ring, err := dnsserver.NewStaticKeyring(keys...)
	check(t, err)
	return ring
}

func TestNotifyExplicitKeyAndCompatibilityDelivery(t *testing.T) {
	for _, mode := range []string{"separate", "transfer-fallback", "explicit-without-transfer", "unsigned"} {
		t.Run(mode, func(t *testing.T) {
			keys := separateNotifyKeys(t, "notify-test-secret")
			key := "notify."
			if mode == "transfer-fallback" {
				key = "key."
			}
			if mode == "unsigned" {
				key = ""
			}
			p := notifyPeerAt(t, "127.0.0.1:0", key, keys)
			policy := notifyPolicy(p.target, true)
			policy.ZoneTransfer = public.AccessAllow
			if mode == "separate" || mode == "explicit-without-transfer" {
				policy.NotifyTSIGKeyRef = "NoTiFy"
			}
			if mode == "unsigned" || mode == "explicit-without-transfer" {
				policy.TransferTSIGKeyRefs = nil
			}
			f := newNotifyFixture(t, policy, keys, netip.Addr{})
			f.change()
			if mode == "separate" {
				_, release, _, model := f.bound()
				if !model.TransferAllowed(netip.MustParseAddr("127.0.0.1"), "key.") || model.TransferAllowed(netip.MustParseAddr("127.0.0.1"), "notify.") {
					t.Fatal("NOTIFY key changed transfer authorization")
				}
				release()
			}
			check(t, f.r.NotifyOnce(f.ctx()))
			if p.calls.Load() != 1 || p.bad.Load() || f.view().Head.Spec.NotifySequence != 0 {
				t.Fatal("selected key did not produce verified delivery and durable ACK")
			}
		})
	}
}

func TestNotifyExplicitKeyMissingRotatedOrRevokedRetainsIntent(t *testing.T) {
	for _, mode := range []string{"missing", "rotation", "revocation", "desired-key", "desired-source"} {
		t.Run(mode, func(t *testing.T) {
			keys := separateNotifyKeys(t, "notify-test-secret")
			p := notifyPeerAt(t, "127.0.0.1:0", "notify.", keys)
			policy := notifyPolicy(p.target, true)
			policy.NotifyTSIGKeyRef = "notify."
			if mode == "missing" {
				keys = separateNotifyKeys(t, "") // transfer key still exists
			}
			f := newNotifyFixture(t, policy, keys, netip.Addr{})
			f.change()
			pending := f.view()
			if mode != "missing" {
				p.setHook(func() {
					if mode == "rotation" || mode == "revocation" {
						secret := "rotated-notify-secret"
						if mode == "revocation" {
							secret = ""
						}
						ctx, cancel := context.WithTimeout(f.ctx(), time.Second)
						defer cancel()
						check(t, f.keys.Replace(ctx, separateNotifyKeys(t, secret)))
					} else {
						z := f.zone.DeepCopy()
						if mode == "desired-key" {
							z.Spec.Policy.NotifyTSIGKeyRef = "key."
						} else {
							z.Spec.Policy.NotifySource = "127.0.0.2"
						}
						_, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: store.ManagerSystem})
						check(t, err)
					}
				})
			}
			if err := f.r.NotifyOnce(f.ctx()); err == nil {
				t.Fatal("unavailable or obsolete NOTIFY choice acknowledged")
			}
			wantCalls := int64(1)
			if mode == "missing" {
				wantCalls = 0
			}
			if p.calls.Load() != wantCalls || p.bad.Load() || !reflect.DeepEqual(pending, f.view()) {
				t.Fatal("refusal used fallback, replayed or changed durable intent")
			}
		})
	}
}

func TestNotifyActualIPv4IPv6SourceSelection(t *testing.T) {
	for _, tc := range []struct {
		name, target, zoneSource, globalSource, expected string
	}{
		{"ipv4-override", "127.0.0.1:0", "127.0.0.2", "127.0.0.1", "127.0.0.2"},
		{"ipv4-global", "127.0.0.1:0", "", "127.0.0.2", "127.0.0.2"},
		{"ipv4-system", "127.0.0.1:0", "", "", "127.0.0.1"},
		{"ipv6-override", "[::1]:0", "::1", "127.0.0.1", "::1"},
		{"ipv6-global", "[::1]:0", "", "::1", "::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := separateNotifyKeys(t, "notify-test-secret")
			p := notifyPeerAt(t, tc.target, "notify.", keys)
			policy := notifyPolicy(p.target, true)
			policy.NotifyTSIGKeyRef, policy.NotifySource = "notify.", tc.zoneSource
			var global netip.Addr
			if tc.globalSource != "" {
				global = netip.MustParseAddr(tc.globalSource)
			}
			f := newNotifyFixture(t, policy, keys, global)
			f.change()
			check(t, f.r.NotifyOnce(f.ctx()))
			source := p.source.Load()
			if p.calls.Load() != 1 || p.bad.Load() || source == nil || *source != netip.MustParseAddr(tc.expected) || f.view().Head.Spec.NotifySequence != 0 {
				t.Fatal("verified NOTIFY did not use the selected source and durable ACK")
			}
		})
	}
}

func TestNotifyUnavailableOrWrongFamilySourceNeverFallsBack(t *testing.T) {
	for _, tc := range []struct{ name, target, source string }{
		{"unavailable-ipv4", "127.0.0.1:0", "192.0.2.123"},
		{"unavailable-ipv6", "[::1]:0", "2001:db8::123"},
		{"wrong-family-v4", "127.0.0.1:0", "::1"},
		{"wrong-family-v6", "[::1]:0", "127.0.0.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := notifyPeerAt(t, tc.target, "key.", ring(t, "test-original-secret"))
			policy := notifyPolicy(p.target, true)
			policy.NotifySource = tc.source
			f := newNotifyFixture(t, policy, ring(t, "test-original-secret"), p.target.Addr())
			f.change()
			pending := f.view()
			if err := f.r.NotifyOnce(f.ctx()); err == nil {
				t.Fatal("unavailable source reported successful delivery")
			}
			if p.calls.Load() != 0 || !reflect.DeepEqual(pending, f.view()) {
				t.Fatal("failed bind fell back to another source or lost intent")
			}
		})
	}
}

func TestNotifySourceChangeBeforeAdmissionSendsNothing(t *testing.T) {
	p := peer(t, true)
	policy := notifyPolicy(p.target, true)
	policy.NotifySource = "127.0.0.2"
	f := newFixture(t, policy)
	f.change()
	pending := f.view()
	ctx, release, _, _ := f.bound()
	defer release()
	z := f.zone.DeepCopy()
	z.Spec.Policy.NotifySource = "127.0.0.3"
	_, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: store.ManagerSystem})
	check(t, err)
	if _, err := f.a.Admission(ctx, pr.Command{Lifecycle: f.l, Expected: pending.Expected, Reason: "notify-delivery"}); err == nil {
		t.Fatal("changed desired source admitted original attempt")
	}
	if p.calls.Load() != 0 || !reflect.DeepEqual(pending, f.view()) {
		t.Fatal("refusal sent or mutated intent")
	}
}
