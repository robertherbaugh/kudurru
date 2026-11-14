package stub

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestManagerCaptureAcquisitionIdlePollingRenewalAndRestore(t *testing.T) {
	c := clock()
	d := &fakeDiscovery{clock: c}
	d.serial.Store(1)
	m, _, dz := setupManager(t, c, d, db(t, filepath.Join(t.TempDir(), "local.db")))
	if initial := m.Capture(); !initial.Current() || initial.Snapshot() == nil || initial.Identity() == "" {
		t.Fatal("new manager has no explicit empty cut")
	}
	stop := run(t, m)
	first := await(t, m, dz, func(s Status) bool { return s.Available })
	held := m.Capture()
	a, b := m.Changes(), m.Changes()
	for range 20 {
		m.Wake()
		time.Sleep(time.Millisecond)
	}
	if !held.Current() || held.Snapshot() != m.Snapshot() || held.Identity() != m.Capture().Identity() {
		t.Fatal("identical scheduler polling churned routes")
	}
	select {
	case <-a:
		t.Fatal("idle manager emitted change")
	default:
	}
	c.advance(10 * time.Second)
	d.serial.Store(2)
	m.Wake()
	renewed := await(t, m, dz, func(s Status) bool { return s.Available && s.Serial == 2 })
	if held.Current() || held.Snapshot().Lookup(zone.StubIdentity(dz)).NextExpiry() != first.ExpiresAt {
		t.Fatal("refresh mutated retained route")
	}
	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Fatal("route change not broadcast")
		}
	}
	stop()
	d.fail.Store(true)
	restarted, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	run(t, restarted)
	restored := await(t, restarted, dz, func(s Status) bool { return !s.ConfirmedAt.IsZero() && s.Error != "" })
	rc := restarted.Capture()
	if !rc.Current() || rc.Identity() == held.Identity() || restored.ConfirmedAt != renewed.ConfirmedAt || restored.ExpiresAt != renewed.ExpiresAt || rc.Snapshot().Lookup(zone.StubIdentity(dz)).RoutingAvailableAt(restored.ExpiresAt) {
		t.Fatal("restore renewed absolute route lifetime or reused installation")
	}
}

func TestManagerCaptureDetachedServersAndExactAdmission(t *testing.T) {
	dz := config("127.0.0.1")
	data := delegation(time.Now())
	z, err := zone.BuildStub(dz, data)
	if err != nil {
		t.Fatal(err)
	}
	e := &entry{id: zone.StubIdentity(dz), z: z, status: Status{Identity: zone.StubIdentity(dz), Servers: data.Servers}}
	m := &Manager{}
	entries := map[string]*entry{e.id: e}
	if err := m.publish(t.Context(), entries); err != nil {
		t.Fatal(err)
	}
	c := m.Capture()
	if err := m.publish(t.Context(), entries); err != nil || !c.Current() {
		t.Fatal("equal route/status contents replaced pointer", err)
	}
	oldProtocol := c.Snapshot().status[0].Servers[0].Protocol
	e.status.Servers[0].Protocol = "tcp"
	if c.Snapshot().status[0].Servers[0].Protocol != oldProtocol {
		t.Fatal("published status retained scheduler-owned slice")
	}
	if err := m.publish(t.Context(), entries); err != nil || c.Current() {
		t.Fatal("changed detached status not installed", err)
	}
	called := false
	fn := func() error { called = true; return nil }
	if err := m.WithCurrent(t.Context(), c, fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("obsolete routes admitted", err)
	}
	other := &Manager{}
	if err := other.publish(t.Context(), entries); err != nil {
		t.Fatal(err)
	}
	if err := m.WithCurrent(t.Context(), other.Capture(), fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("foreign routes admitted", err)
	}
	var absent *Manager
	if absent.Capture().Snapshot() != nil || absent.Capture().Current() || absent.Changes() != nil {
		t.Fatal("nil manager fabricated capability")
	}
	if err := absent.WithCurrent(t.Context(), m.Capture(), fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("nil manager admitted callback", err)
	}
	if err := m.WithCurrent(t.Context(), m.Capture(), fn); err != nil || !called {
		t.Fatal("exact routes denied", err)
	}
}
