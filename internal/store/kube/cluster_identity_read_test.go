package kube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func startupIdentityStore(t *testing.T, handler http.HandlerFunc, options Options, background bool) *Store {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer identity-fixture" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/v1/namespaces/kube-system" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	mapper, err := directMapper()
	if err != nil {
		t.Fatal(err)
	}
	core := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	core.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	options.Logger = testLogger()
	st, err := newStore(&rest.Config{Host: server.URL, Transport: server.Client().Transport, BearerToken: "identity-fixture"}, options, background, meta.MultiRESTMapper{core, mapper})
	if err != nil {
		t.Fatal("construct original authenticated store", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

func TestStartupClusterIDSharedObservedDiscovery(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	st := startupIdentityStore(t, func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"actual-observed"}}`))
	}, Options{StartupTimeout: time.Second}, true)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("original discovery did not enter")
	}
	type result struct {
		id  string
		err error
	}
	done := make(chan result, 8)
	for range 8 {
		go func() { id, err := st.StartupClusterID(t.Context()); done <- result{id, err} }()
	}
	select {
	case <-done:
		t.Fatal("getter invented identity before authenticated observation")
	default:
	}
	unblock()
	for range 8 {
		select {
		case got := <-done:
			if got.err != nil || got.id != "actual-observed" {
				t.Fatal("shared original observation unavailable", got.err)
			}
		case <-time.After(time.Second):
			t.Fatal("original waiters did not join")
		}
	}
	if id, err := st.StartupClusterID(t.Context()); err != nil || id != "actual-observed" || reads.Load() != 1 {
		t.Fatal("getter repeated discovery or lost observed identity", err)
	}
}

func TestStartupClusterIDOriginalBoundsAndClose(t *testing.T) {
	for _, mode := range []string{"configured bound", "earlier deadline", "cancellation", "close", "empty UID", "direct"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			var reads atomic.Int32
			bound := time.Second
			if mode == "configured bound" || mode == "empty UID" {
				bound = 40 * time.Millisecond
			}
			st := startupIdentityStore(t, func(w http.ResponseWriter, r *http.Request) {
				if reads.Add(1) == 1 {
					close(entered)
				}
				if mode == "empty UID" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system"}}`))
					return
				}
				<-r.Context().Done()
			}, Options{StartupTimeout: bound}, mode != "direct")
			if mode != "direct" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("discovery did not enter")
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "earlier deadline" {
				var earlier context.CancelFunc
				ctx, earlier = context.WithTimeout(ctx, 20*time.Millisecond)
				defer earlier()
			}
			if mode == "cancellation" {
				cancel()
			}
			type result struct {
				id  string
				err error
			}
			done := make(chan result, 1)
			go func() { id, err := st.StartupClusterID(ctx); done <- result{id, err} }()
			if mode == "close" {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var got result
			select {
			case got = <-done:
			case <-time.After(time.Second):
				t.Fatal("original getter did not end")
			}
			want := error(context.DeadlineExceeded)
			switch mode {
			case "cancellation":
				want = context.Canceled
			case "close":
				want = store.ErrClosed
			case "direct":
				want = errIdentityUnavailable
			}
			if got.id != "" || !errors.Is(got.err, want) {
				t.Fatal("unobserved identity or wrong original failure", got.err)
			}
			if mode == "direct" && reads.Load() != 0 {
				t.Fatal("direct getter invented background discovery")
			}
			if mode == "empty UID" {
				select {
				case <-st.clusterIDReady:
					t.Fatal("empty namespace UID notified readiness")
				default:
				}
			}
		})
	}
}

func TestStartupClusterIDPinnedObservation(t *testing.T) {
	for _, uid := range []string{"expected", "other"} {
		t.Run(uid, func(t *testing.T) {
			var reads atomic.Int32
			st := startupIdentityStore(t, func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"` + uid + `"}}`))
			}, Options{ExpectedClusterID: "expected", StartupTimeout: time.Second}, false)
			if st.Info().ClusterID != "" {
				t.Fatal("configured pin represented an observation")
			}
			id, err := st.StartupClusterID(t.Context())
			if uid == "expected" {
				if err != nil || id != uid {
					t.Fatal("observed pin unavailable", err)
				}
			} else if id != "" || !errors.Is(err, ErrClusterIdentityMismatch) {
				t.Fatal("mismatching observed identity was accepted", err)
			}
			_, _ = st.StartupClusterID(t.Context())
			if reads.Load() != 1 {
				t.Fatal("getter reprobed completed original pin")
			}
		})
	}
}

func TestStartupClusterIDPinnedCloseCancelsOriginalProbe(t *testing.T) {
	entered, finished := make(chan struct{}), make(chan struct{})
	st := startupIdentityStore(t, func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(finished)
	}, Options{ExpectedClusterID: "expected", StartupTimeout: time.Second}, false)
	done := make(chan error, 1)
	go func() {
		id, err := st.StartupClusterID(t.Context())
		if id != "" {
			err = errors.New("closed probe invented an observed identity")
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("original authenticated probe did not enter")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrClosed) {
			t.Fatal("closed original probe was accepted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closed getter did not join")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Store close stranded original authenticated probe")
	}
}

func TestStartupClusterIDActualAPIServer(t *testing.T) {
	requireEnv(t)
	var namespace corev1.Namespace
	if err := shared.admin.Get(t.Context(), client.ObjectKey{Name: metav1.NamespaceSystem}, &namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.UID == "" {
		t.Fatal("actual API server omitted namespace identity")
	}
	for _, pinned := range []bool{false, true} {
		name := "discovered"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			opts := Options{Logger: testLogger(), StartupTimeout: 2 * time.Second}
			if pinned {
				opts.ExpectedClusterID = string(namespace.UID)
			}
			st, err := New(shared.cfg, opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			id, err := st.StartupClusterID(t.Context())
			if err != nil || id != string(namespace.UID) {
				t.Fatal("getter lost actual authenticated namespace identity", err)
			}
		})
	}
}
