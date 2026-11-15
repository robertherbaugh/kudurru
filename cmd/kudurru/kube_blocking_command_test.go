package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/config"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

type kubeBlockingWireFaults struct {
	writes    atomic.Int32
	drop      atomic.Bool
	afterRead atomic.Pointer[func()]
}

type kubeBlockingWire struct {
	next   http.RoundTripper
	faults *kubeBlockingWireFaults
}

func (w *kubeBlockingWire) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := w.next.RoundTrip(r)
	if strings.Contains(r.URL.Path, "/configmaps") {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			w.faults.writes.Add(1)
			if err == nil && response.StatusCode < 300 && w.faults.drop.Swap(false) {
				_ = response.Body.Close()
				return nil, errors.New("actual committed blocking response lost")
			}
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+kube.BlockingRuntimeName) {
			if fn := w.faults.afterRead.Swap(nil); fn != nil {
				(*fn)()
			}
		}
	}
	return response, err
}

func TestKubeBlockingCommandActualUnknownAndOriginalExpiry(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "kustomize", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	cfg, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	faults := &kubeBlockingWireFaults{}
	wire := rest.CopyConfig(cfg)
	wire.Wrap(func(next http.RoundTripper) http.RoundTripper { return &kubeBlockingWire{next: next, faults: faults} })
	st, err := kube.New(wire, kube.Options{Namespace: "default", Identity: "blocking-command", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	host, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	toggle := blockingstage.NewToggle(nil)
	run := newKubeBlockingRuntime(st, toggle, config.NewReloadable(host), apitest.Authorizer(apitest.StandardBindings()...))
	ctx := auth.WithPrincipal(t.Context(), auth.Principal{Kind: auth.KindUser, Name: "admin", AuthMethod: "token"})
	if err := run.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	faults.drop.Store(true)
	result, err := run.Command(ctx, api.BlockingChange{HasTimer: true, TimerSeconds: 60})
	var problem *api.Error
	if result != nil || !errors.As(err, &problem) || problem.Status != http.StatusServiceUnavailable || problem.Body.Hint.(map[string]any)["outcome"] != "unknown" || faults.writes.Load() != 1 || !toggle.Enabled() {
		t.Fatal("unknown command outcome was retried or applied locally", result, err)
	}
	// An independent read can observe the actual commit, without changing the
	// original command's unknown outcome or resubmitting it.
	if err := run.refresh(ctx); err != nil || toggle.Enabled() {
		t.Fatal("independent committed-state observation failed", err)
	}
	before := run.observed.Load()
	start := time.Now()
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	principal := auth.WithCredentialTimeBounds(auth.Principal{Kind: auth.KindUser, Name: "admin", AuthMethod: "token"}, start, start.Add(-time.Second), start.Add(time.Second), func() time.Time { return time.Unix(0, clock.Load()) })
	expiring := auth.WithPrincipal(t.Context(), principal)
	expire := func() { clock.Store(start.Add(2 * time.Second).UnixNano()) }
	faults.afterRead.Store(&expire)
	_, err = run.Command(expiring, api.BlockingChange{Blocking: true})
	if !errors.As(err, &problem) || problem.Status != http.StatusUnauthorized || faults.writes.Load() != 1 || toggle.Enabled() {
		t.Fatal("original credential expiry after source read permitted a write", err)
	}
	if err := run.refresh(ctx); err != nil || run.observed.Load().ResourceVersion != before.ResourceVersion {
		t.Fatal("expired command changed persisted state", err)
	}
	run.gate <- struct{}{}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = run.Command(canceled, api.BlockingChange{Blocking: true})
	<-run.gate
	if err == nil || faults.writes.Load() != 1 {
		t.Fatal("canceled queued command dispatched", err)
	}
}

func TestKubeBlockingObservationDoesNotRearmConsumedDeadline(t *testing.T) {
	now := time.Now()
	toggle := blockingstage.NewToggle(func() time.Time { return now })
	run := &kubeBlockingRuntime{toggle: toggle}
	state := kube.BlockingRuntimeState{UID: "original", ResourceVersion: "1", Until: now.Add(time.Second)}
	run.apply(state)
	now = now.Add(2 * time.Second)
	if !toggle.Enabled() {
		t.Fatal("original deadline did not expire")
	}
	now = now.Add(-2 * time.Second)
	state.ResourceVersion = "2" // An unrelated ConfigMap field was changed.
	run.apply(state)
	if !toggle.Enabled() {
		t.Fatal("same absolute deadline was rearmed by another observation")
	}
}
