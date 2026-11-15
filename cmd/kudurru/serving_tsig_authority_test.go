package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// This is one actual API request retained across supported promotion. It does
// not identify which private guard returns the established refusal status.
func TestServingTSIGHeldFollowerHTTPAcrossPromotion(t *testing.T) {
	primary := newTSIGCommandFixture(t)
	primaryAPI := tsigCommandHTTP(t, primary.servingCommandFixture, nil)
	created := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, primaryAPI, primary.token, http.MethodPost, "/tsigkeys", tsigCreateBody("held-replica.", 0x71)), http.StatusCreated)
	setup, cancelSetup := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelSetup()
	follower, image := tsigABQNewFollower(setup, t, primary)
	if tsigABQAwaitRuntime(setup, follower.runtime, false) != nil {
		t.Fatal("original follower did not publish complete initialized state")
	}
	adapter, err := servinghttp.New(servinghttp.Options{Publisher: follower.runtime.publisher, Now: follower.runtime.opts.Now})
	if err != nil {
		t.Fatal("prepare actual follower HTTP adapter")
	}
	entered := make(chan serving.Identity, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := tsigCommandHTTP(t, &servingCommandFixture{s: follower.runtime, http: adapter}, func(r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tsigkeys/held-replica./replace" {
			return
		}
		identity, captureErr := servinghttp.Identity(r.Context())
		if captureErr != nil {
			return
		}
		entered <- identity
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	metadata := tsigRouteRequest(setup, t, server, primary.token, http.MethodGet, "/tsigkeys", nil)
	if metadata.Code != http.StatusOK {
		t.Fatal("initialized follower metadata was unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	done := tsigABQStartRequest(ctx, t, server, primary.token, "/tsigkeys/held-replica./replace", tsigReplaceBody(created.Key, 0x72))
	joined := false
	t.Cleanup(func() {
		cancel()
		unblock()
		if !joined {
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				t.Error("original held follower request did not join")
			}
		}
	})
	select {
	case original := <-entered:
		if !original.Base.Same(image.Generation()) {
			t.Fatal("held HTTP request did not retain the original follower image")
		}
	case <-ctx.Done():
		t.Fatal("single original follower request did not reach its capture gate")
	}
	promoted, err := follower.store.Promote(ctx, replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: image.Generation(), RequestID: "tsig-held-original-promotion"})
	if err != nil {
		t.Fatal("supported follower promotion failed")
	}
	follower.startWriter(t)
	if tsigABQAwaitRuntime(ctx, follower.runtime, true) != nil || !follower.runtime.publisher.Status().Served.Base.Term.Same(promoted.Term) {
		t.Fatal("promoted original writer did not become completely ready within the held request")
	}
	before, err := follower.store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal("read promoted generation before original request release")
	}
	auditBefore := tsigABQAudit(ctx, t, follower.store, 0)
	rowBefore := tsigABQImageRow(t, before, "held-replica.")
	if rowBefore == nil || string(rowBefore.UID) != created.Key.UID {
		t.Fatal("promoted image lost the original durable credential incarnation")
	}
	unblock()
	select {
	case response := <-done:
		joined = true
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), `"committed":true`) {
			t.Fatal("held follower request adopted promoted command authority")
		}
	case <-ctx.Done():
		t.Fatal("original held follower request exceeded its original bound")
	}
	after, err := follower.store.ReadGeneration(ctx)
	if err != nil || !tsigABQSameImage(before, after) || !reflect.DeepEqual(rowBefore, tsigABQImageRow(t, after, "held-replica.")) {
		t.Fatal("refused original request changed the promoted durable image")
	}
	if !reflect.DeepEqual(auditBefore, tsigABQAudit(ctx, t, follower.store, 0)) || follower.store.VerifyAudit(ctx) != nil {
		t.Fatal("refused original request changed the promoted audit baseline")
	}
}

type tsigABQFollower struct {
	store        *sqlite.Store
	runtime      *servingRuntime
	rings        *servingbuild.KeyringOwner
	life         context.Context
	cancelRun    context.CancelFunc
	done         chan error
	writerCancel context.CancelFunc
	writerDone   chan error
}

func tsigABQNewFollower(ctx context.Context, t *testing.T, primary *tsigCommandFixture) (*tsigABQFollower, *replication.Image) {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal("prepare initialized replica scheme")
	}
	path := filepath.Join(t.TempDir(), "held-follower.db")
	open := func() *sqlite.Store {
		st, openErr := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: primary.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "tsig-held-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if openErr != nil {
			t.Fatal("open original held follower")
		}
		return st
	}
	f := &tsigABQFollower{store: open()}
	// One cleanup deadline covers original writer, complete Run, owner retirement
	// and Close. A timed-out join never clears its handle or reports idle.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		join := func(done <-chan error) bool {
			if done == nil {
				return true
			}
			select {
			case runErr := <-done:
				if runErr != nil && !errors.Is(runErr, context.Canceled) {
					t.Error("held follower worker returned an unexpected error")
				}
				return true
			case <-ctx.Done():
				t.Error("held follower worker did not join within the shared cleanup bound")
				return false
			}
		}
		// Production order (cluster.Runner): cancel and join the elected writer,
		// then cancel the always-on runtime it writes through.
		if f.writerCancel != nil {
			f.writerCancel()
		}
		writerJoined := join(f.writerDone)
		if writerJoined && f.writerDone != nil && f.runtime.life.Err() != nil {
			t.Error("held follower always-on runtime stopped before its writer joined")
		}
		if f.cancelRun != nil {
			f.cancelRun()
		}
		rootJoined := join(f.done)
		if !writerJoined || !rootJoined {
			return
		}
		if f.runtime != nil && f.runtime.Close(ctx) != nil {
			t.Error("held follower complete runtime did not close")
		}
		if f.rings != nil && f.rings.Close(ctx) != nil {
			t.Error("held follower immutable ring did not close")
		}
		if f.store.Close() != nil {
			t.Error("held follower store did not close")
		}
	})
	if f.store.Enroll(ctx, primary.s.opts.Store.Authority().Term) != nil {
		t.Fatal("enroll original complete follower")
	}
	if _, err := f.store.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal("bind original follower anchor lifecycle")
	}
	ctx, err = f.store.CaptureAnchorInstall(ctx)
	if err != nil {
		t.Fatal("capture original follower before source image")
	}
	image, err := primary.s.opts.Store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal("read original complete replica image")
	}
	validated, err := f.store.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal("validate original complete replica")
	}
	installed, err := f.store.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil || !installed.Same(image.Generation()) || f.store.Close() != nil {
		t.Fatal("install and close original complete follower")
	}
	f.store = open()
	f.rings, err = servingbuild.NewKeyringOwner(primary.source.baseline)
	if err != nil {
		t.Fatal("prepare original follower file baseline")
	}
	source, err := newTSIGRuntime(f.store, primary.s.opts.AuthKeys, f.rings, primary.source.baseline)
	if err != nil || source.prepare(ctx) != nil {
		t.Fatal("prepare original follower encrypted source after restart")
	}
	opts := primary.s.opts
	opts.Store, opts.Keyrings, opts.TSIG = f.store, f.rings, source
	opts.Transferred = servingbuild.NewTransferredRuntime(nil, nil, nil)
	f.runtime, err = newServingRuntime(ctx, opts)
	if err != nil {
		t.Fatal("prepare real follower complete runtime")
	}
	// The cleanup above cancels and joins the writer before it cancels this
	// always-on context; the end of t.Context() must not reverse that order
	// (servingFixtureAlwaysOn).
	f.life, f.cancelRun = servingFixtureAlwaysOn(t)
	f.done = make(chan error, 1)
	go func() { f.done <- f.runtime.Run(f.life) }()
	return f, image
}

func (f *tsigABQFollower) startWriter(t *testing.T) {
	t.Helper()
	if f.writerDone != nil {
		t.Fatal("held follower fixture attempted a second writer")
	}
	// The writer's election context is not a child of the always-on context,
	// as in cluster.Runner, so the writer ends through its own term only.
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	f.writerCancel, f.writerDone = cancel, make(chan error, 1)
	go func() {
		f.writerDone <- f.store.Elector("tsig-held-promoted-writer").Run(ctx, func(term context.Context) {
			if err := f.runtime.RunWriter(term); err != nil && term.Err() == nil && f.runtime.life.Err() == nil {
				t.Error("original promoted writer failed")
			}
		}, nil)
	}()
}

func tsigABQAwaitRuntime(ctx context.Context, s *servingRuntime, writer bool) error {
	// Driver readiness has no subscription. Poll only its read-only status under
	// the existing original context; this timer does not establish quiet causality.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		changed, authority := s.publisher.Subscribe(), s.publisher.AuthorityChanges()
		if s.publisher.Status().ProtectedReady && (!writer || s.primary.driver.Status().Ready && s.primary.writer.current(s.primary.writer.active.Load())) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-authority:
		case <-tick.C:
		}
	}
}

func tsigABQStartRequest(ctx context.Context, t *testing.T, handler http.Handler, token, path string, body any) <-chan *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal("encode bounded original route request")
	}
	request := httptest.NewRequest(http.MethodPost, "http://kudurru.test/api/v1"+path, bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		done <- response
	}()
	return done
}

func tsigABQAudit(ctx context.Context, t *testing.T, st *sqlite.Store, after int64) []sqlite.AuditEntry {
	t.Helper()
	rows, err := st.ReadAudit(ctx, sqlite.AuditQuery{AfterID: after, Limit: 256})
	if err != nil || len(rows) >= 256 {
		t.Fatal("bounded complete fixture audit unavailable")
	}
	return rows
}

func tsigABQSameImage(before, after *replication.Image) bool {
	return before != nil && after != nil && before.Generation().Same(after.Generation()) && before.ObjectResourceVersion == after.ObjectResourceVersion && reflect.DeepEqual(before.KindManifest, after.KindManifest) && reflect.DeepEqual(before.Payload, after.Payload)
}
