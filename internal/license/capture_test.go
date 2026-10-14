package license

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestCapturedLicensePollingReplacementAndOriginalDeadline(t *testing.T) {
	f := newFixture(t)
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	m, err := New(Options{Keyring: f.ring, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	empty := m.Capture()
	if !empty.Current() || empty.Identity() == "" || empty.Status().Allows(FeatureOIDC) {
		t.Fatal("explicit community cut is not a valid denying policy")
	}
	raw := f.key(t, func(c *Claims) { c.ExpiresAt = t0.Add(time.Hour).Unix() })
	if err := m.Set(raw, "test"); err != nil {
		t.Fatal(err)
	}
	cut := m.Capture()
	changed := m.Changes()
	if empty.Current() || !cut.Current() || !cut.Status().Allows(FeatureOIDC) {
		t.Fatal("capture did not bind the new installation")
	}
	status := cut.Status()
	status.Features[0] = "fake"
	*status.GraceEndsAt = t0
	if !cut.Current() || cut.Status().Features[0] == "fake" || cut.Status().GraceEndsAt.Equal(t0) {
		t.Fatal("captured status aliases output")
	}
	if err := m.Set(raw, "test"); err != nil {
		t.Fatal(err)
	}
	if !cut.Current() || cut.Identity() != m.Capture().Identity() {
		t.Fatal("identical polling rotated authority")
	}
	select {
	case <-changed:
		t.Fatal("identical polling signaled replacement")
	default:
	}
	clock.Store(t0.Add(time.Hour).UnixNano())
	if !cut.Current() || !m.Capture().Status().Allows(FeatureOIDC) {
		t.Fatal("grace incorrectly removed management authority")
	}
	clock.Store(t0.Add(time.Hour + Grace).UnixNano())
	if cut.Current() || m.Capture().Status().Allows(FeatureOIDC) || cut.Identity() == m.Capture().Identity() {
		t.Fatal("original license deadline extended or denying interval reused identity")
	}
	clock.Store(t0.UnixNano())
	if err := m.Set("", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(raw, "test"); err != nil {
		t.Fatal(err)
	}
	if cut.Current() || cut.Identity() == m.Capture().Identity() {
		t.Fatal("identical-byte reactivation revived old installation")
	}
}

func TestCapturedLicenseNotBeforeReadFailureAndClockRegression(t *testing.T) {
	f := newFixture(t)
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	m, err := New(Options{Keyring: f.ring, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	raw := f.key(t, func(c *Claims) { c.NotBefore = t0.Add(time.Minute).Unix() })
	if err := m.Set(raw, "test"); err != nil {
		t.Fatal(err)
	}
	before := m.Capture()
	if !before.Current() || before.Status().Allows(FeatureOIDC) {
		t.Fatal("future license did not produce explicit denying cut")
	}
	clock.Store(t0.Add(time.Minute).UnixNano())
	active := m.Capture()
	if before.Current() || !active.Current() || !active.Status().Allows(FeatureOIDC) || active.Identity() == before.Identity() {
		t.Fatal("not-before transition reused denied policy interval")
	}
	clock.Store(t0.UnixNano())
	if active.Current() {
		t.Fatal("clock regression accepted a later capture")
	}
	clock.Store(t0.Add(time.Minute).UnixNano())
	changed := m.Changes()
	err = m.Load([]Source{{Name: "test", Read: func() (string, error) { return "", errors.New("offline") }}})
	if err == nil || active.Current() || m.Capture().Status().Allows(FeatureOIDC) {
		t.Fatal("read failure retained captured authority")
	}
	select {
	case <-changed:
	default:
		t.Fatal("read failure did not announce replacement")
	}
	if (Cut{}).Current() || (*Manager)(nil).Capture().Current() || new(Manager).Capture().Current() {
		t.Fatal("missing installation admitted")
	}
}

func TestLicensePinSpansActualSQLiteCommitAndNoop(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(map[bool]string{false: "noop", true: "changed"}[mutate], func(t *testing.T) {
			m := Unrestricted()
			st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			var cut Cut
			go func() {
				done <- m.WithCurrent(t.Context(), func(c Cut) error {
					cut = c
					return st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						if mutate {
							if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "test", Data: []byte(`{}`)}); err != nil {
								return err
							}
						}
						return tx.RequireCommitCheck(func(context.Context) error {
							close(entered)
							<-release
							if !c.Current() {
								return ErrCapture
							}
							return nil
						})
					})
				})
			}()
			<-entered
			replaced := make(chan error, 1)
			go func() { replaced <- m.Set("", "") }()
			select {
			case err := <-replaced:
				close(release)
				<-done
				t.Fatal("replacement crossed final commit barrier", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := <-replaced; err != nil || cut.Current() {
				t.Fatal("replacement did not invalidate after complete command", err)
			}
			image, err := st.ReadGeneration(t.Context())
			if err != nil || (len(image.Payload.Runtime) == 1) != mutate {
				t.Fatal("unexpected command state", err)
			}
		})
	}
}

func TestLicensePinCancellationDoesNotEnterCallback(t *testing.T) {
	m := Unrestricted()
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := m.WithCurrent(ctx, func(Cut) error { t.Fatal("canceled pin entered"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("pin did not inherit cancellation", err)
	}
}

func TestLicenseDeadlineAtFinalCheckRollsBackSQLite(t *testing.T) {
	f := newFixture(t)
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	m, err := New(Options{Keyring: f.ring, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set(f.key(t, func(c *Claims) { c.ExpiresAt = t0.Add(time.Hour).Unix() }), "test"); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	before, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = m.WithCurrent(t.Context(), func(c Cut) error {
		return st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "test", Data: []byte(`{}`)}); err != nil {
				return err
			}
			return tx.RequireCommitCheck(func(context.Context) error {
				clock.Store(t0.Add(time.Hour + Grace).UnixNano())
				if !c.Current() {
					return ErrCapture
				}
				return nil
			})
		})
	})
	if !errors.Is(err, ErrCapture) {
		t.Fatal("expired policy authorized staged command", err)
	}
	after, err := st.ReadGeneration(t.Context())
	if err != nil || !after.Generation().Same(before.Generation()) {
		t.Fatal("expired final check did not roll back entire command", err)
	}
}
