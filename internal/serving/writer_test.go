package serving

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWriterGateAllowsPendingRecoveryWithoutReadAdmission(t *testing.T) {
	s := primary(t)
	p, err := New(Options{Source: s, Required: CompleteManifest(), Prepare: plainPrepare})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if lease, err := p.CaptureHTTP(t.Context()); !errors.Is(err, ErrPending) || lease != nil {
		t.Fatal("unpublished HTTP admitted", err)
	}
	authority := s.Authority()
	target := TermActivation{Term: authority.Term, Incarnation: authority.Incarnation}
	called := 0
	err = p.WithWriterGate(t.Context(), target, func() error {
		called++
		return s.Command(t.Context(), target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := tx.Apply(ctx, &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "recovery"}, Spec: v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypePrimary, ZoneName: "recovery.example."}}, store.ApplyOptions{Manager: store.ManagerAPI})
			return err
		})
	})
	if err != nil || called != 1 {
		t.Fatal("pending recovery failed", called, err)
	}
	if lease, err := p.CaptureHTTP(t.Context()); !errors.Is(err, ErrPending) || lease != nil {
		t.Fatal("writer gate made an unpublished owner readable", err)
	}
	for _, wrong := range []TermActivation{
		{Term: target.Term, Incarnation: target.Incarnation + 1},
		{Term: replication.Term{ClusterID: target.Term.ClusterID, Epoch: target.Term.Epoch + 1, PrimaryID: target.Term.PrimaryID, PromotionID: "other"}, Incarnation: target.Incarnation},
	} {
		if err := p.WithWriterGate(t.Context(), wrong, func() error { t.Error("stale writer entered"); return nil }); !errors.Is(err, ErrSuperseded) {
			t.Fatal("wrong writer accepted", err)
		}
	}
	if _, err := s.ObserveAuthority(t.Context(), replication.Term{ClusterID: target.Term.ClusterID, Epoch: target.Term.Epoch + 1, PrimaryID: "other", PromotionID: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := p.WithWriterGate(t.Context(), target, func() error { t.Error("fenced writer entered"); return nil }); !errors.Is(err, ErrSuperseded) {
		t.Fatal("fenced writer accepted", err)
	}
}

func TestWriterGateExcludesPointerSwapAndPromotionPin(t *testing.T) {
	s := primary(t)
	applyPair(t, s, "old")
	prepared := make(chan struct{}, 1)
	p := runPublisher(t, Options{Source: s, Prepare: func(_ context.Context, c Candidate) (Prepared, error) {
		parts, err := allComponents(c)
		select {
		case prepared <- struct{}{}:
		default:
		}
		return parts, err
	}})
	old := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	<-prepared
	a := s.Authority()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	done := make(chan error, 1)
	denied := errors.New("callback result")
	go func() {
		done <- p.WithWriterGate(t.Context(), TermActivation{Term: a.Term, Incarnation: a.Incarnation}, func() error {
			close(entered)
			<-release
			return denied
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer gate never entered")
	}
	applyPair(t, s, "new")
	select {
	case <-prepared:
	case <-time.After(5 * time.Second):
		t.Fatal("new candidate never prepared")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, unpin, err := p.Pin(ctx); !errors.Is(err, context.DeadlineExceeded) || unpin != nil {
		t.Fatal("promotion crossed writer gate", err)
	}
	if got := p.Status(); !got.Served.Base.Same(old.Served.Base) || got.ProtectedReady {
		t.Fatal("publication crossed writer gate or admitted stale HTTP")
	}
	lease, err := p.Capture()
	if err != nil {
		t.Fatal("last-good DNS capture blocked", err)
	}
	lease.Release()
	finish()
	if err := <-done; err != denied {
		t.Fatal("writer gate changed callback result", err)
	}
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && !s.Served.Base.Same(old.Served.Base) })
}
