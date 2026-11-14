package iamgate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/iammigrate"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type sourceFunc func(context.Context) (iammigrate.Status, *iammigrate.Bindings, error)

func (f sourceFunc) Capture(ctx context.Context) (iammigrate.Status, *iammigrate.Bindings, error) {
	return f(ctx)
}

func TestAdmissionNeverReusesEarlierSuccess(t *testing.T) {
	status := iammigrate.Status{State: iammigrate.Native, RemoteAllowed: true}
	var fault error
	calls := 0
	g := New(sourceFunc(func(ctx context.Context) (iammigrate.Status, *iammigrate.Bindings, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("unbounded admission check")
		}
		return status, nil, fault
	}))
	ctx, _, err := g.Capture(context.Background())
	if err != nil || ctx.Err() != nil {
		t.Fatal("native admission", err)
	}
	status = iammigrate.Status{State: iammigrate.Running}
	if _, st, err := g.Capture(ctx); !errors.Is(err, ErrClosed) || st.RemoteAllowed {
		t.Fatal("reused request's former native proof")
	}
	status = iammigrate.Status{State: iammigrate.Native, RemoteAllowed: true}
	fault = errors.New("private credential-like detail")
	if _, st, err := g.Capture(ctx); !errors.Is(err, ErrClosed) || st.RemoteAllowed || strings.Contains(err.Error(), "credential-like") {
		t.Fatal("outage reused native admission or leaked details")
	}
	if calls != 3 {
		t.Fatal("unexpected source calls", calls)
	}
}

func TestAdmissionRejectsIncompleteOrMismatchedProof(t *testing.T) {
	for _, status := range []iammigrate.Status{
		{State: iammigrate.Running, RemoteAllowed: true},
		{State: iammigrate.Complete, RemoteAllowed: true, JournalResourceVersion: "rv", MigrationID: "id"},
		{State: iammigrate.Native, RemoteAllowed: true, JournalResourceVersion: "rv"},
		{State: iammigrate.Native, RemoteAllowed: true, MigrationID: "id"},
	} {
		g := New(sourceFunc(func(context.Context) (iammigrate.Status, *iammigrate.Bindings, error) { return status, nil, nil }))
		if _, st, err := g.Capture(context.Background()); !errors.Is(err, ErrClosed) || st.RemoteAllowed {
			t.Fatal("inconsistent admission accepted", status)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	g := New(sourceFunc(func(context.Context) (iammigrate.Status, *iammigrate.Bindings, error) {
		return iammigrate.Status{State: iammigrate.Native, RemoteAllowed: true}, nil, nil
	}))
	if _, _, err := g.Capture(canceled); !errors.Is(err, ErrClosed) {
		t.Fatal("canceled admission accepted")
	}
	if _, ok := ResolveToken(context.Background(), nil, nil); ok {
		t.Fatal("missing request proof fell back to live data")
	}
}

func TestAdmissionWorkIsBoundedAndDoesNotRunHandlerWhileClosed(t *testing.T) {
	entered, release := make(chan struct{}, 8), make(chan struct{})
	g := New(sourceFunc(func(ctx context.Context) (iammigrate.Status, *iammigrate.Bindings, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return iammigrate.Status{State: iammigrate.Native, RemoteAllowed: true}, nil, nil
	}))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, _, _ = g.Capture(context.Background()) })
	}
	for range 8 {
		<-entered
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	w := httptest.NewRecorder()
	g.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("overloaded gate ran login") })).ServeHTTP(w, r)
	close(release)
	wg.Wait()
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "iam_migration_required") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("gate response: %d %s", w.Code, w.Body.String())
	}
}

type writer struct {
	store.Store
	mutations int
}

func (s *writer) Apply(_ context.Context, o store.Object, _ store.ApplyOptions) (store.Object, error) {
	s.mutations++
	return o, nil
}
func (s *writer) Delete(context.Context, store.Key, store.DeleteOptions) error {
	s.mutations++
	return nil
}
func (s *writer) UpdateStatus(context.Context, store.Object) error { s.mutations++; return nil }
func (s *writer) Restore(context.Context, io.Reader) error         { s.mutations++; return nil }

func TestCurrentIAMWritesCloseWhileDNSConfigurationContinues(t *testing.T) {
	allowed := true
	g := New(sourceFunc(func(context.Context) (iammigrate.Status, *iammigrate.Bindings, error) {
		return iammigrate.Status{State: iammigrate.Native, RemoteAllowed: allowed}, nil, nil
	}))
	ctx, _, err := g.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	base := &writer{}
	s := &Store{Store: base, Gate: g}
	sa := &publicv1.ServiceAccount{TypeMeta: metav1.TypeMeta{Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "dns"}}
	allowed = false
	if _, err := s.Apply(ctx, sa, store.ApplyOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatal("stale request admitted IAM Apply")
	}
	if err := s.UpdateStatus(ctx, sa); !errors.Is(err, ErrClosed) {
		t.Fatal("stale request admitted IAM status")
	}
	if err := s.Delete(ctx, store.KeyOf(sa), store.DeleteOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatal("stale request admitted IAM delete")
	}
	if err := s.Restore(ctx, strings.NewReader("")); !errors.Is(err, ErrClosed) {
		t.Fatal("closed restore admitted")
	}
	if base.mutations != 0 {
		t.Fatal("IAM writes reached underlying store")
	}
	zone := &publicv1.DNSZone{TypeMeta: metav1.TypeMeta{Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "home"}}
	if _, err := s.Apply(ctx, zone, store.ApplyOptions{}); err != nil || base.mutations != 1 {
		t.Fatal("identity migration disabled core DNS writes", err)
	}
	allowed = true
	if _, err := s.Apply(ctx, sa, store.ApplyOptions{}); err != nil || base.mutations != 2 {
		t.Fatal("native IAM write blocked", err)
	}
}
