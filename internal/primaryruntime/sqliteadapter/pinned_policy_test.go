package sqliteadapter

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

func TestActualUpdateRequiresSamePinPolicyAndOptionalConjunct(t *testing.T) {
	f := setup(t)
	ctx := f.ctx()
	l := f.active("primary", "example.")
	request := f.request(l, "pinned-policy", "dynamic.example. 60 IN A 192.0.2.1")
	before := snapshot(t, f.s)
	validator := f.keys.current.Policy
	f.keys.current.Policy = nil
	if out := f.r.Apply(ctx, request); out.Rcode != dns.RcodeServerFailure || out.Committed || !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("optional check substituted for mandatory validator", out)
	}
	f.keys.current.Policy = validator
	f.a.o.Policy = func(context.Context, *sqlite.CommandTx, *public.DNSZone, zonestate.Policy) error {
		return pr.ErrUnavailable
	}
	if out := f.r.Apply(ctx, request); out.Rcode != dns.RcodeServerFailure || out.Committed || !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("mandatory validator bypassed optional denial", out)
	}
	f.a.o.Policy = nil
	if out := f.r.Apply(ctx, request); out.Rcode != dns.RcodeSuccess || !out.Committed {
		t.Fatal("same-pin validator unnecessarily required another capture", out)
	}
	before = snapshot(t, f.s)
	f.keys.current.Policy = func(context.Context, *sqlite.CommandTx, *public.DNSZone, zonestate.Policy) error {
		return pr.ErrUnavailable
	}
	if out := f.r.Apply(ctx, request); out.Rcode != dns.RcodeServerFailure || !errors.Is(out.Err, pr.ErrUnavailable) || !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("receipt replay bypassed current pinned policy", out)
	}
	if out := f.r.Apply(ctx, f.request(l, "pinned-noop", "dynamic.example. 60 IN A 192.0.2.1")); out.Rcode == dns.RcodeSuccess || !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("semantic no-op bypassed current pinned policy", out)
	}
}
