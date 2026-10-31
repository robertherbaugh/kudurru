package secondary

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestManagerCaptureAcquisitionIdlePollingRenewalAndRestore(t *testing.T) {
	clock := newManagerClock()
	rx := &managerReceiver{serial: 1}
	m, _, dz := managerFor(t, clock, rx, managerDB(t))
	if c := m.Capture(); !c.Current() || c.Identity() == "" || c.Snapshot() == nil {
		t.Fatal("new manager omitted explicit empty installation")
	}
	stop := runManager(t, m)
	first := awaitManager(t, m, dz, func(s Status) bool { return s.Available })
	c := m.Capture()
	a, b := m.Changes(), m.Changes()
	for range 20 {
		m.Wake()
		time.Sleep(time.Millisecond)
	}
	if !c.Current() || c.Snapshot() != m.Snapshot() || m.Capture().Identity() != c.Identity() {
		t.Fatal("identical scheduler polling churned actual installation")
	}
	select {
	case <-a:
		t.Fatal("idle polling emitted dependency wake")
	default:
	}
	rx.set(2, nil)
	clock.advance(10 * time.Second)
	m.Wake()
	renewed := awaitManager(t, m, dz, func(s Status) bool { return s.Serial == 2 && s.Available })
	if c.Current() || c.Snapshot().Lookup(zone.SecondaryIdentity(dz)).NextExpiry() != first.ExpiresAt {
		t.Fatal("renewal revived or mutated held cut")
	}
	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Fatal("renewal did not broadcast")
		}
	}
	stop()
	rx.set(2, errors.New("offline"))
	restarted, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	runManager(t, restarted)
	restored := awaitManager(t, restarted, dz, func(s Status) bool { return !s.ConfirmedAt.IsZero() && s.Error != "" })
	rc := restarted.Capture()
	if rc.Identity() == c.Identity() || !rc.Current() || restored.ConfirmedAt != renewed.ConfirmedAt || restored.ExpiresAt != renewed.ExpiresAt || rc.Snapshot().Lookup(zone.SecondaryIdentity(dz)).AvailableAt(restored.ExpiresAt) {
		t.Fatal("restoration changed confirmation/expiry or reused installation")
	}
}

func TestManagerCapturePointerEqualityDetachedProvenanceAndAdmission(t *testing.T) {
	dz := managerZone()
	model, err := zone.BuildSecondary(dz, managerRecords(1), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e := &entry{id: zone.SecondaryIdentity(dz), z: model, data: &envelope{Identity: zone.SecondaryIdentity(dz), Credential: "acquired-A"}, status: Status{Identity: zone.SecondaryIdentity(dz)}}
	m := &Manager{}
	entries := map[string]*entry{e.id: e}
	if err := m.publish(t.Context(), entries); err != nil {
		t.Fatal(err)
	}
	c := m.Capture()
	if err := m.publish(t.Context(), entries); err != nil || !c.Current() {
		t.Fatal("equal zone POINTER/status/provenance maps changed", err)
	}
	e.credential = "expected-B"
	if err := m.publish(t.Context(), entries); err != nil || !c.Current() {
		t.Fatal("expected credentials relabeled unchanged acquired copy", err)
	}
	e.data.Credential = "acquired-B"
	if err := m.publish(t.Context(), entries); err != nil || c.Current() || c.Snapshot().credentials[e.id] != "acquired-A" {
		t.Fatal("changed acquired provenance not detached/installed", err)
	}
	called := false
	fn := func() error { called = true; return nil }
	if err := m.WithCurrent(t.Context(), c, fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("obsolete cut admitted", err)
	}
	var nilManager *Manager
	if nilManager.Capture().Current() || nilManager.Changes() != nil || nilManager.Capture().Snapshot() != nil {
		t.Fatal("nil manager fabricated an empty capability")
	}
	if err := nilManager.WithCurrent(t.Context(), m.Capture(), fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("nil manager entered callback", err)
	}
	other := &Manager{}
	if err := other.publish(t.Context(), entries); err != nil {
		t.Fatal(err)
	}
	if err := m.WithCurrent(t.Context(), other.Capture(), fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("wrong manager cut admitted", err)
	}
}

func TestManagerCapturePinsWholeSQLitePublicationWhileAcquisitionContinues(t *testing.T) {
	clock := newManagerClock()
	rx := &managerReceiver{serial: 1}
	blobs := managerDB(t)
	m, _, dz := managerFor(t, clock, rx, blobs)
	runManager(t, m)
	first := awaitManager(t, m, dz, func(s Status) bool { return s.Available })
	c := m.Capture()
	changed := m.Changes()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_, token, err := s.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	var published *Snapshot
	go func() {
		done <- m.WithCurrent(t.Context(), c, func() error {
			return s.PublishIfCurrent(t.Context(), token, sqlite.PublicationOptions{}, func() error {
				close(entered)
				<-release
				if !c.Current() {
					return snapshotowner.ErrObsolete
				}
				published = c.Snapshot()
				return nil
			})
		})
	}()
	<-entered
	rx.set(2, nil)
	clock.advance(10 * time.Second)
	m.Wake()
	// A real acquisition and blob write can complete while publication pins the
	// previous installation. Neither network nor persistence takes this gate.
	until := time.Now().Add(5 * time.Second)
	acquired := false
	for time.Now().Before(until) {
		blob, err := blobs.GetBlob(t.Context(), "secondary/"+zone.SecondaryIdentity(dz))
		var data envelope
		if err == nil && json.Unmarshal(blob.Data, &data) == nil && data.ConfirmedAt.After(first.ConfirmedAt) {
			acquired = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !acquired || rx.count() != 2 || !c.Current() || c.Snapshot() != m.Snapshot() {
		t.Fatal("acquisition blocked by pin or installation crossed SQLite barrier")
	}
	select {
	case <-changed:
		t.Fatal("manager installed during whole SQLite callback")
	default:
	}
	unblock()
	if err := <-done; err != nil || published != c.Snapshot() {
		t.Fatal("SQLite published a different captured installation", err)
	}
	awaitManager(t, m, dz, func(s Status) bool { return s.Available && s.Serial == 2 })
	if c.Current() || m.Capture().Identity() == c.Identity() {
		t.Fatal("completed new installation failed to invalidate published cursor")
	}
	select {
	case <-changed:
	default:
		t.Fatal("actual replacement did not wake whole publisher")
	}
}

func TestManagerCapturePinsFullSQLiteCommandIncludingCommitCheck(t *testing.T) {
	m := &Manager{}
	if err := m.publish(t.Context(), map[string]*entry{}); err != nil {
		t.Fatal(err)
	}
	c := m.Capture()
	changed := m.Changes()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- m.WithCurrent(t.Context(), c, func() error {
			return s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "capture-test", Data: json.RawMessage(`{}`)}); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(context.Context) error {
					close(entered)
					<-release
					if !c.Current() {
						return snapshotowner.ErrObsolete
					}
					return nil
				})
			})
		})
	}()
	<-entered
	installed := make(chan error, 1)
	go func() {
		installed <- m.publish(t.Context(), map[string]*entry{"new": {status: Status{Identity: "new"}}})
	}()
	select {
	case err := <-installed:
		t.Fatal("manager replacement crossed actual final commit check", err)
	case <-time.After(20 * time.Millisecond):
	}
	if !c.Current() {
		t.Fatal("command's pinned cursor changed")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-installed; err != nil || c.Current() {
		t.Fatal("replacement after durable commit", err)
	}
	image, _, err := s.ReadServingCandidate(t.Context())
	if err != nil || len(image.Payload.Runtime) != 1 {
		t.Fatal("actual command did not durably commit", err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("command release omitted replacement wake")
	}
}
