package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func writeControl(t *testing.T, f *fixture, c config.Control) {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal("control fixture encoding failed")
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		settings.Spec.Settings["control"] = extensions.JSON{Raw: data}
		_, err := tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	var saved public.Kudurru
	if err := f.store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &saved); err != nil {
		t.Fatal("control readback failed")
	}
	var restored config.Control
	if json.Unmarshal(saved.Spec.Settings["control"].Raw, &restored) != nil || len(restored.AllowedNetworks) != len(c.AllowedNetworks) || len(restored.BlockedQTypes) != len(c.BlockedQTypes) {
		t.Fatal("generated settings CRD pruned control configuration")
	}
}

func controlRequest(t *testing.T, lease *serving.Lease) *pipeline.Request {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion("blocked.example.", dns.TypeA)
	m.SetEdns0(1232, true)
	r := &pipeline.Request{Ctx: t.Context(), Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:12345"), Proto: pipeline.ProtoUDP}}
	if lease != nil {
		clone, err := lease.Clone()
		if err != nil {
			t.Fatal("control lease clone failed")
		}
		r.Serving = servingdns.Wrap(clone)
		t.Cleanup(r.Serving.Release)
	}
	return r
}

func waitControl(t *testing.T, p *serving.Publisher, want control.Action, minimumRevision ...uint64) *serving.Lease {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		changed := p.Subscribe()
		lease, err := p.Capture()
		if err == nil {
			value, ok := lease.Bundle().Component("control")
			view, typed := value.(*control.View)
			if ok && typed && view != nil && (len(minimumRevision) == 0 || lease.Bundle().Identity().Base.Revision >= minimumRevision[0]) && view.Decide(controlRequest(t, nil)) == want {
				return lease
			}
			lease.Release()
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("exact control publication did not converge")
		}
	}
}

func TestControlCompleteLeaseKeepsOriginalView(t *testing.T) {
	f := newFixture(t)
	old := waitControl(t, f.pub, control.Allow)
	defer old.Release()
	writeControl(t, f, config.Control{BlockedQTypes: []string{"A"}})
	fresh := waitControl(t, f.pub, control.Refuse, f.store.Authority().Installed.Revision)
	defer fresh.Release()
	oldValue, _ := old.Bundle().Component("control")
	freshValue, _ := fresh.Bundle().Component("control")
	if oldValue == freshValue {
		t.Fatal("replacement reused original mutable control view")
	}
	for _, test := range []struct {
		lease  *serving.Lease
		rcode  int
		status pipeline.Status
	}{{old, dns.RcodeSuccess, pipeline.StatusBlockedExact}, {fresh, dns.RcodeRefused, pipeline.StatusRefusedACL}} {
		resp, err := f.chain.Serve(controlRequest(t, test.lease))
		if err != nil || resp == nil {
			t.Fatal("captured control chain failed")
		}
		defer resp.Close()
		if resp.Msg.Rcode != test.rcode || resp.Status != test.status {
			t.Fatalf("captured control changed request cut: rcode=%d status=%d", resp.Msg.Rcode, resp.Status)
		}
	}
	// A network-only replacement must not exempt ordinary non-loopback clients.
	writeControl(t, f, config.Control{AllowedNetworks: []string{"198.51.100.0/24"}})
	network := waitControl(t, f.pub, control.Refuse, f.store.Authority().Installed.Revision)
	defer network.Release()
	// The previously captured allowed request stays allowed after both replacements.
	resp, err := f.chain.Serve(controlRequest(t, old))
	if err != nil || resp == nil || resp.Msg.Rcode != dns.RcodeSuccess {
		t.Fatal("successor network rules repaired original request")
	}
	resp.Close()
}

func TestControlMissingOriginalOwnerCannotUseSuccessor(t *testing.T) {
	f := newFixture(t)
	var valid atomic.Bool
	p := catalogReadStartPublisher(t, f.store, func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
		prepared, err := f.builder.Prepare(ctx, c)
		if !valid.Load() {
			for i := range prepared.Components {
				if prepared.Components[i].Name == "control" {
					prepared.Components[i].Value = struct{}{}
				}
			}
		}
		return prepared, err
	})
	bad, err := p.Capture()
	if err != nil {
		t.Fatal("malformed owner fixture capture failed")
	}
	defer bad.Release()
	chain := newChain(t, p)
	valid.Store(true)
	writeControl(t, f, config.Control{})
	fresh := waitControl(t, p, control.Allow)
	defer fresh.Release()
	if resp, err := chain.Serve(controlRequest(t, bad)); resp != nil || !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("missing original control owner used successor/live rules")
	}
	resp, err := chain.Serve(controlRequest(t, fresh))
	if err != nil || resp == nil || resp.Msg.Rcode != dns.RcodeSuccess {
		t.Fatal("valid successor control owner did not serve")
	}
	resp.Close()
}
