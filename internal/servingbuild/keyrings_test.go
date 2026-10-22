package servingbuild

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func ring(t *testing.T, value string) *dnsserver.StaticKeyring {
	t.Helper()
	keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updates.example.", Algorithm: dnsserver.TSIGHMACSHA256, Secret: []byte(strings.Repeat(value, 32))})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestKeyringCutsStayImmutableAndNeverReviveAfterRotation(t *testing.T) {
	first, second := ring(t, "a"), ring(t, "b")
	o, err := NewKeyringOwner(first)
	if err != nil {
		t.Fatal(err)
	}
	old := o.Capture()
	changed := o.Changes()
	if err := o.Replace(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("rotation did not notify publisher")
	}
	middle := o.Capture()
	if old.Current() || !middle.Current() || old.Keys() != first || middle.Keys() != second {
		t.Fatal("rotation mixed captured keys")
	}
	if err := o.Replace(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	last := o.Capture()
	if old.Current() || middle.Current() || !last.Current() || old.Identity() == last.Identity() {
		t.Fatal("restored key bytes revived a stale cut")
	}
	if err := o.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if last.Current() || o.Capture().Keys() != nil || last.Keys() != first {
		t.Fatal("close lost held immutable keys or retained current authority")
	}
	if err := o.Replace(t.Context(), second); !errors.Is(err, serving.ErrClosed) {
		t.Fatal("closed owner accepted replacement", err)
	}
}

func TestPrimaryKeyringPinCoversActualCommitCheckAndRotation(t *testing.T) {
	first, second := ring(t, "a"), ring(t, "b")
	o, err := NewKeyringOwner(first)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, o)
	a := f.store.Authority()
	fence := pr.PublicationFence{Term: a.Term, Incarnation: a.Incarnation}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	done := make(chan error, 1)
	old := o.Capture()
	go func() {
		done <- o.PrimaryPin(f.pub)(t.Context(), fence, func(p currentguard.Pinned) error {
			if p.Keys != first || p.Identity != old.Identity() {
				return errors.New("wrong pinned generation")
			}
			return f.store.Command(t.Context(), fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.RequireCommitCheck(func(context.Context) error {
					close(entered)
					<-release
					if !old.Current() {
						return errors.New("replacement crossed actual precommit check")
					}
					return nil
				}); err != nil {
					return err
				}
				_, err := tx.Apply(ctx, settings("192.0.2.12"), store.ApplyOptions{Manager: store.ManagerSystem})
				return err
			})
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("command never reached final admission")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := o.Replace(ctx, second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("rotation crossed full command pin", err)
	}
	if _, release, err := f.pub.Pin(ctx); !errors.Is(err, context.DeadlineExceeded) || release != nil {
		t.Fatal("promotion pin crossed command pin", err)
	}
	finish()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := o.Replace(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	if old.Current() || o.Capture().Keys() != second {
		t.Fatal("postcommit rotation lost")
	}
}

func TestKeyringGuardCoversActualSQLitePublicationCallback(t *testing.T) {
	o, err := NewKeyringOwner(ring(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, o)
	_, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source, err := o.GuardSource(f.store)
	if err != nil {
		t.Fatal(err)
	}
	old := o.Capture()
	err = source.PublishIfCurrent(t.Context(), token, sqlite.PublicationOptions{}, func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if err := o.Replace(ctx, ring(t, "b")); !errors.Is(err, context.DeadlineExceeded) {
			return errors.New("replacement crossed SQLite publication barrier")
		}
		if !old.Current() {
			return errors.New("candidate dependency changed before pointer swap")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeNotifyKeysRetainCompleteDependencyCut(t *testing.T) {
	first, second := ring(t, "a"), ring(t, "b")
	o, err := NewKeyringOwner(first)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, o)
	capture := func() (string, *dnsserver.StaticKeyring, func() bool) {
		t.Helper()
		lease, err := f.pub.CaptureHTTP(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(lease.Release)
		value, _ := lease.Bundle().Component("runtime")
		runtime, ok := value.(*RuntimeOwner)
		if !ok {
			t.Fatal("missing complete runtime owner")
		}
		return runtime.NotifyKeys()
	}
	oldID, oldKeys, oldCurrent := capture()
	if oldID != o.Capture().Identity() || oldKeys != first || oldCurrent == nil || !oldCurrent() {
		t.Fatal("runtime did not retain its prepared keyring")
	}
	for _, keys := range []*dnsserver.StaticKeyring{second, first} {
		if err := o.Replace(t.Context(), keys); err != nil {
			t.Fatal(err)
		}
		f.waitCurrent(t)
		id, got, current := capture()
		if id != o.Capture().Identity() || id == oldID || got != keys || !current() || oldCurrent() || oldKeys != first {
			t.Fatal("replacement mixed complete owners or revived an old NOTIFY cut")
		}
	}
	_, _, current := capture()
	// Current includes non-key dependencies, even though the keys remain valid.
	f.epoch.Add(1)
	if current() || !o.Capture().Current() {
		t.Fatal("NOTIFY predicate ignored another changed dependency")
	}
}
