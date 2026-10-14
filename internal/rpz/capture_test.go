package rpz

import (
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
)

func TestManagerCaptureAcquisitionIdlePollingRenewalAndRestore(t *testing.T) {
	rx := &testReceiver{serial: 1}
	fx := newRuntime(t, rx, runtimeDB(t), runtimeList())
	if empty := fx.m.Capture(); !empty.Current() || empty.Identity() == "" || empty.Snapshot() == nil {
		t.Fatal("new manager omitted explicit empty snapshot")
	}
	stop := startRuntime(t, fx.m)
	first := awaitStatus(t, fx, func(s Status) bool { return s.Available })
	c := fx.m.Capture()
	a, b := fx.m.Changes(), fx.m.Changes()
	for range 20 {
		fx.m.Wake()
		time.Sleep(time.Millisecond)
	}
	if !c.Current() || c.Snapshot() != fx.m.Snapshot() || c.Identity() != fx.m.Capture().Identity() {
		t.Fatal("identical scheduler polling churned RPZ feeds")
	}
	select {
	case <-a:
		t.Fatal("idle RPZ polling emitted dependency wake")
	default:
	}
	rx.set(2, nil)
	fx.clock.add(10 * time.Second)
	fx.m.Wake()
	renewed := awaitStatus(t, fx, func(s Status) bool { return s.Available && s.Serial == 2 })
	if c.Current() || c.Snapshot().Lookup(SourceIdentity(fx.list, 0)).AvailableAt(first.ExpiresAt) {
		t.Fatal("renewal changed held feed lifetime")
	}
	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Fatal("real RPZ replacement not broadcast")
		}
	}
	stop()
	rx.set(2, errors.New("offline"))
	restarted, err := New(fx.m.opts)
	if err != nil {
		t.Fatal(err)
	}
	fx.m = restarted
	startRuntime(t, restarted)
	restored := awaitStatus(t, fx, func(s Status) bool { return !s.ConfirmedAt.IsZero() && s.Error != "" })
	rc := restarted.Capture()
	if rc.Identity() == c.Identity() || !rc.Current() || restored.ConfirmedAt != renewed.ConfirmedAt || restored.ExpiresAt != renewed.ExpiresAt || rc.Snapshot().Lookup(SourceIdentity(fx.list, 0)).AvailableAt(restored.ExpiresAt) {
		t.Fatal("restore changed actual absolute feed validity or identity")
	}
}

func TestManagerCaptureDetachedProvenanceAndExactAdmission(t *testing.T) {
	list := runtimeList()
	spec, cfg, err := sourceConfig(list, 0)
	if err != nil {
		t.Fatal(err)
	}
	spec.ExpiresAt = time.Now().Add(time.Hour)
	z, err := Compile(spec, runtimeRecords(1))
	if err != nil {
		t.Fatal(err)
	}
	e := &entry{spec: spec, dz: cfg, z: z, data: &envelope{Identity: spec.Identity, Credential: "acquired-A"}, status: Status{Identity: spec.Identity}}
	m := &Manager{}
	entries := map[string]*entry{spec.Identity: e}
	if err := m.publish(t.Context(), entries); err != nil {
		t.Fatal(err)
	}
	c := m.Capture()
	if err := m.publish(t.Context(), entries); err != nil || !c.Current() {
		t.Fatal("equal feed POINTER/status/credential maps churned", err)
	}
	e.credential = "expected-B"
	if err := m.publish(t.Context(), entries); err != nil || !c.Current() {
		t.Fatal("expected credential relabeled acquired A", err)
	}
	e.data.Credential = "acquired-B"
	if err := m.publish(t.Context(), entries); err != nil || c.Current() || c.Snapshot().credentials[spec.Identity] != "acquired-A" {
		t.Fatal("published provenance not detached/replaced", err)
	}
	called := false
	fn := func() error { called = true; return nil }
	if err := m.WithCurrent(t.Context(), c, fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("obsolete feed cut admitted", err)
	}
	other := &Manager{}
	if err := other.publish(t.Context(), entries); err != nil {
		t.Fatal(err)
	}
	if err := m.WithCurrent(t.Context(), other.Capture(), fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("foreign feed cut admitted", err)
	}
	var absent *Manager
	if absent.Capture().Snapshot() != nil || absent.Capture().Current() || absent.Changes() != nil {
		t.Fatal("nil manager fabricated feed capability")
	}
	if err := absent.WithCurrent(t.Context(), m.Capture(), fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("nil manager entered callback", err)
	}
	if err := m.WithCurrent(t.Context(), m.Capture(), fn); err != nil || !called {
		t.Fatal("exact feed cut denied", err)
	}
}
