package providercoordinator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestEntitlementWithdrawalRegainFloorsAndRejectsOldProof(t *testing.T) {
	server := newProviderServer(t)
	f := newFixture(t, server)
	accept(t, f, candidates(t, f, 1))
	ready(t, f, 1)
	oldProof := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, f.host.now().Add(-time.Minute))}
	oldView := f.view(t)
	if _, err := oldView.Authenticate(t.Context(), oldProof, f.host.now()); err != nil {
		t.Fatal(err)
	}
	f.host.install(&hostGeneration{id: "entitlement-withdrawn", allowed: false, secret: "test-only"})
	wait(t, func() bool {
		image, _ := f.store.ReadGeneration(t.Context())
		capture, err := f.coordinator.Capture(t.Context(), image)
		if err != nil {
			return false
		}
		defer capture.Release()
		return capture.HostIdentity == "entitlement-withdrawn"
	})
	ctx, target := writer(t, f.store)
	if err := f.coordinator.admit(ctx, target, nil); err != nil {
		t.Fatal("explicit entitlement withdrawal failed", err)
	}
	for _, r := range f.registry(t).Authorities() {
		if r.Active {
			t.Fatal("entitlement loss retained active receipt")
		}
	}
	image, _ := f.store.ReadGeneration(t.Context())
	s, err := servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil || len(s.ProviderFloors) != 1 {
		t.Fatal("entitlement withdrawal omitted durable floor", err)
	}
	floor := s.ProviderFloors[0]
	f.host.install(&hostGeneration{id: "entitlement-regained", allowed: true, secret: "test-only"})
	accept(t, f, candidates(t, f, 1))
	ready(t, f, 1)
	if _, err := f.view(t).Authenticate(t.Context(), oldProof, f.host.now()); !isDenied(err) {
		t.Fatal("regain revived proof issued below persistent floor", err)
	}
	f.clock.Add(int64(2 * time.Second))
	newProof := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, f.host.now())}
	if _, err := f.view(t).Authenticate(t.Context(), newProof, f.host.now()); err != nil {
		t.Fatal("new proof after regain denied", err)
	}
	image, _ = f.store.ReadGeneration(t.Context())
	s, err = servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil || len(s.ProviderFloors) != 1 || s.ProviderFloors[0].ValidAfter.Before(floor.ValidAfter) {
		t.Fatal("regain lowered persistent revocation floor", err)
	}
	// A held old immutable read remains its exact cut; it never authorizes current
	// commands. Current proof verification above reads the new floor.
	if _, err := oldView.Authenticate(t.Context(), oldProof, f.host.now()); err != nil {
		t.Fatal("held old owner mutated after withdrawal", err)
	}
}

func TestAuthorityAdmissionPinHostThroughFinalCommitAndDeadlineRollback(t *testing.T) {
	server := newProviderServer(t)
	f := newFixture(t, server)
	cs := candidates(t, f, 1)
	ctx, target := writer(t, f.store)
	entered, release := make(chan struct{}), make(chan struct{})
	original := f.coordinator.opts.Admit
	f.coordinator.opts.Admit = func(ctx context.Context, tx *sqlite.CommandTx, a *Admission) error {
		if err := original(ctx, tx, a); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { close(entered); <-release; return nil })
	}
	result := make(chan error, 1)
	go func() { result <- f.coordinator.admit(ctx, target, cs) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("authority command did not reach final hook")
	}
	swapped := make(chan struct{})
	go func() {
		f.host.install(&hostGeneration{id: "independent-authority-replacement", allowed: true, secret: "test-only"})
		close(swapped)
	}()
	select {
	case <-swapped:
		t.Fatal("authority host replacement crossed final hook")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	select {
	case <-swapped:
	case <-time.After(time.Second):
		t.Fatal("authority host pin leaked")
	}
	deadline := f.host.now().Add(time.Second)
	f.host.install(&hostGeneration{id: "authority-deadline", allowed: true, secret: "test-only", deadline: deadline})
	cs = candidates(t, f, 1)
	before, _ := f.store.ReadGeneration(t.Context())
	f.coordinator.opts.Admit = func(ctx context.Context, tx *sqlite.CommandTx, a *Admission) error {
		if err := original(ctx, tx, a); err != nil {
			return err
		}
		f.clock.Store(deadline.UnixNano())
		return nil
	}
	if err := f.coordinator.admit(ctx, target, cs); !errors.Is(err, ErrStale) {
		t.Fatal("expired authority admitted", err)
	}
	after, _ := f.store.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("expired receipt/floor command did not roll back")
	}
}

func TestCaptureRejectsCapsAndMalformedRegistryBeforeHostAllocation(t *testing.T) {
	f := newFixture(t)
	o := f.coordinator.opts
	var captures atomic.Int64
	o.Host = func(ctx context.Context, r *providerregistry.Registry) (HostCapture, error) {
		captures.Add(1)
		return f.host.capture(ctx, r)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	image, _ := f.store.ReadGeneration(t.Context())
	for _, corrupt := range []func(*replication.Image){
		func(i *replication.Image) { i.Payload.Security = make([]replication.Entry, servingauth.MaxRows+1) },
		func(i *replication.Image) {
			i.Payload.Security = []replication.Entry{{Key: "too-large", Data: json.RawMessage(`"` + strings.Repeat("x", servingauth.MaxRootBytes) + `"`)}}
		},
		func(i *replication.Image) {
			i.Payload.Security = append([]replication.Entry(nil), image.Payload.Security...)
			for j, r := range i.Payload.Security {
				if r.Key == providerregistry.RootKey {
					i.Payload.Security[j].Data = json.RawMessage(strings.Replace(string(r.Data), `"formatVersion":1`, `"formatVersion":99`, 1))
				}
			}
			if err := replication.Seal(i); err != nil {
				t.Fatal(err)
			}
		},
	} {
		bad := *image
		corrupt(&bad)
		if _, err := c.Capture(t.Context(), &bad); err == nil {
			t.Fatal("oversized or malformed provider cut accepted")
		}
	}
	if captures.Load() != 0 {
		t.Fatal("invalid complete input allocated a host capability")
	}
}
