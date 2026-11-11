package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type tsigCommandFixture struct {
	*servingCommandFixture
	source      *tsigRuntime
	stopSource  func()
	startSource func()
}

// This component fixture exercises actual routes and current transactions with
// one controllable source; mounted tests separately cover production Run.
// Build the original source before workers; never race a live opts.TSIG assignment.
func newTSIGCommandFixture(t *testing.T) *tsigCommandFixture {
	t.Helper()
	f := &servingCommandFixture{s: newServingRuntimeFixture(t), now: time.Now().UTC().Truncate(time.Second)}
	f.expires = f.now.Add(time.Hour)
	f.installClock(func() time.Time { return f.now.Add(time.Duration(f.offset.Load())) })
	var id string
	f.token, id = tokens.Generate()
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			return err
		}
		created := metav1.NewTime(f.now.Add(-time.Minute))
		user, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "root-tsig", CreationTimestamp: created}, Spec: internalv1.LocalUserSpec{BuiltinAdmin: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		expires := metav1.NewTime(f.expires)
		_, err = tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: created}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(f.token), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:root-tsig", Provider: "local"}, Scopes: []string{"admin"}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal("seed original fixture authentication")
	}
	source, err := newTSIGRuntime(f.s.opts.Store, f.s.opts.AuthKeys, f.s.opts.Keyrings, f.s.opts.Keyrings.Capture().Keys())
	if err != nil {
		t.Fatal("construct original TSIG source")
	}
	f.s.opts.TSIG = source
	if source.prepare(t.Context()) != nil {
		t.Fatal("prepare explicit original TSIG source")
	}
	result := &tsigCommandFixture{servingCommandFixture: f, source: source}
	result.startSource, result.stopSource = runTSIGCommandFixtureWorkers(t, f.s)
	result.startSource()
	writerCtx, cancelWriter := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- f.s.opts.Store.Elector("tsig-management-original").Run(writerCtx, func(ctx context.Context) {
			if err := f.s.RunWriter(ctx); err != nil && ctx.Err() == nil && f.s.life.Err() == nil {
				t.Error("original TSIG fixture writer failed")
			}
		}, nil)
	}()
	var once sync.Once
	f.stop = func() {
		once.Do(func() {
			cancelWriter()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error("original TSIG fixture writer did not join cleanly")
				}
			case <-time.After(10 * time.Second):
				t.Error("original TSIG fixture writer did not join")
			}
		})
	}
	t.Cleanup(f.stop)
	awaitServingRuntime(t, f.s, func() bool { return f.s.primary.driver.Status().Ready && f.s.publisher.Status().ProtectedReady })
	f.http, err = servinghttp.New(servinghttp.Options{Publisher: f.s.publisher, Now: f.currentClock()})
	if err != nil {
		t.Fatal("prepare original protected HTTP adapter")
	}
	return result
}

// The seven core workers use their real production methods and nested joins.
// Only the original TSIG source can pause; opts.TSIG and writer ownership stay
// fixed. Do not call s.Run here, since it would start an uncontrolled source.
func runTSIGCommandFixtureWorkers(t *testing.T, s *servingRuntime) (start, stop func()) {
	t.Helper()
	if s.opts.TSIG == nil {
		t.Fatal("original TSIG fixture source missing")
	}
	// These workers stand in for s.Run, so they end only in the cleanup below
	// and never with t.Context() (servingFixtureAlwaysOn).
	parent, cancel := servingFixtureAlwaysOn(t)
	bound, finish, err := s.begin(parent, false)
	if err != nil {
		cancel()
		t.Fatal("register original TSIG fixture workers")
	}
	g, ctx := errgroup.WithContext(bound)
	control := &tsigTestSourceControl{ctx: ctx, life: s.life, source: s.opts.TSIG}
	var shutdownOnce sync.Once
	var shutdownDeadline time.Time
	shutdown := func() time.Time {
		shutdownOnce.Do(func() {
			shutdownDeadline = time.Now().Add(10 * time.Second)
			control.mu.Lock()
			control.shuttingDown = true
			control.mu.Unlock()
			cancel()
		})
		return shutdownDeadline
	}
	control.cancelAll = func() { shutdown() }
	g.Go(func() error { return s.publisher.Run(ctx) })
	g.Go(func() error { return s.providers.Run(ctx) })
	g.Go(func() error { return s.integrations.host.Run(ctx) })
	g.Go(func() error { return s.catalog.Watch(ctx, s.publisher.Wake) })
	g.Go(func() error { return s.opts.Transferred.Watch(ctx, s.publisher.Wake) })
	g.Go(func() error {
		for {
			changed := s.opts.Keyrings.Changes()
			s.publisher.Wake()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
			}
		}
	})
	g.Go(func() error { return s.observeHost(ctx) })
	done := make(chan struct{})
	go func() {
		err := g.Wait()
		shutdown()
		control.mu.Lock()
		episode := control.episode
		control.mu.Unlock()
		if episode != nil {
			// Keep the real registration until actual completion, even if the
			// bounded cleanup has already reported an unjoined child.
			<-episode.done
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			control.fail()
			t.Error("original TSIG fixture core workers exited unexpectedly")
		}
		s.cancel()
		finish()
		close(done)
	}()
	// Registered before writer cleanup: LIFO joins the original writer first.
	t.Cleanup(func() {
		deadline := shutdown()
		sourceDeadline := time.Now().Add(5 * time.Second)
		if deadline.Before(sourceDeadline) {
			sourceDeadline = deadline
		}
		if err := control.stop(sourceDeadline); err != nil {
			t.Error(err)
		}
		// Source and core joins share this one original total budget. A
		// timeout never closes done, releases registration or permits restart.
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			control.fail()
			t.Error("original TSIG fixture workers did not join within total budget")
		}
	})
	return func() {
			t.Helper()
			control.start(t)
		}, func() {
			t.Helper()
			if err := control.stop(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
		}
}

type tsigTestSourceEpisode struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	err    error // written before done closes; never exposed as raw diagnostics
}

type tsigTestSourceControl struct {
	mu           sync.Mutex
	ctx          context.Context
	life         context.Context
	source       *tsigRuntime
	cancelAll    func()
	episode      *tsigTestSourceEpisode
	terminal     bool
	shuttingDown bool
}

func (c *tsigTestSourceControl) fail() {
	c.mu.Lock()
	c.terminal = true
	c.mu.Unlock()
	c.cancelAll()
}

func (c *tsigTestSourceControl) start(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	if c.episode != nil || c.terminal || c.shuttingDown || c.ctx.Err() != nil || c.life.Err() != nil {
		c.mu.Unlock()
		t.Fatal("original TSIG source cannot start before join or after lifetime end")
	}
	ctx, cancel := context.WithCancel(c.ctx)
	episode := &tsigTestSourceEpisode{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	c.episode = episode
	c.mu.Unlock()
	go func() {
		episode.err = c.source.Run(episode.ctx)
		if episode.ctx.Err() == nil || episode.err != nil && !errors.Is(episode.err, context.Canceled) {
			c.fail()
			t.Error("original TSIG source exited unexpectedly")
		}
		close(episode.done)
	}()
}

func (c *tsigTestSourceControl) stop(deadline time.Time) error {
	c.mu.Lock()
	episode := c.episode
	c.mu.Unlock()
	if episode == nil {
		return nil
	}
	episode.cancel()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-episode.done:
		c.mu.Lock()
		terminal := c.terminal
		if !terminal && c.episode == episode {
			c.episode = nil // clear only after positive join, never on timeout
		}
		c.mu.Unlock()
		if terminal || episode.err != nil && !errors.Is(episode.err, context.Canceled) {
			return errors.New("original TSIG source fixture failed terminally")
		}
		return nil
	case <-timer.C:
		c.fail() // retain the original unjoined handle and refuse all restarts
		return errors.New("original TSIG source did not join within original budget")
	}
}

func tsigCommandHTTP(t *testing.T, f *servingCommandFixture, before func(*http.Request)) *api.Server {
	t.Helper()
	srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, TSIGKeyReads: f.s.TSIGKeyReads, TSIGKeyCommand: f.s.TSIGKeyCommand, Authenticate: func(next http.Handler) http.Handler {
		return f.http.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if before != nil {
				before(r)
			}
			next.ServeHTTP(w, r)
		}))
	}, Authorizer: f.http, Now: f.currentClock(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal("mount actual TSIG management routes")
	}
	t.Cleanup(srv.Close)
	return srv
}

func tsigRouteRequest(ctx context.Context, t *testing.T, server *api.Server, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal("encode bounded TSIG route input")
		}
	}
	request := httptest.NewRequest(method, "http://kudurru.test/api/v1"+path, bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	return result
}

func tsigReadReceipt(t *testing.T, response *httptest.ResponseRecorder, status int) api.TSIGKeyCommandReceipt {
	t.Helper()
	var receipt api.TSIGKeyCommandReceipt
	if response.Code != status || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || !receipt.Committed || receipt.Published != (status != http.StatusAccepted) {
		t.Fatalf("TSIG receipt invalid: status=%d want=%d", response.Code, status)
	}
	if strings.Contains(response.Body.String(), "sealed") || strings.Contains(response.Body.String(), "credentialEpoch") || strings.Contains(response.Body.String(), "secret") {
		t.Fatal("TSIG receipt disclosed private fields")
	}
	return receipt
}

func tsigCreateBody(name string, value byte) map[string]any {
	return map[string]any{"keyName": name, "algorithm": "hmac-sha256", "secret": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))}
}

func tsigReplaceBody(metadata api.TSIGKeyMetadata, value byte) map[string]any {
	return map[string]any{"uid": metadata.UID, "resourceVersion": metadata.ResourceVersion, "secret": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))}
}

func tsigReadPrivateRow(t *testing.T, st *sqlite.Store, name string) *kinds.TSIGKey {
	t.Helper()
	row := new(kinds.TSIGKey)
	if st.Get(t.Context(), store.Key{Kind: kinds.KindTSIGKey, Name: kinds.ObjectName(name)}, row) != nil {
		t.Fatal("read actual encrypted TSIG row")
	}
	return row
}

func TestServingTSIGActualManagementLifecycleAndOriginalEpoch(t *testing.T) {
	f := newTSIGCommandFixture(t)
	server := tsigCommandHTTP(t, f.servingCommandFixture, nil)
	held, err := f.s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal("retain original management planning cut")
	}
	defer held.Release()
	originalRaw, _ := held.Bundle().Component("runtime")
	originalOwner := originalRaw.(*servingbuild.RuntimeOwner)
	original := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys", tsigCreateBody("stored.", 0x61)), http.StatusCreated)
	first, _ := f.s.opts.Keyrings.Capture().Keys().Identity("stored.")
	_, _, oldFullCurrent := originalOwner.NotifyKeys()
	if oldFullCurrent() || !originalOwner.TSIGOriginalCurrent() {
		t.Fatal("own commit did not separate old ring currency from original host/provider lifetime")
	}
	replaced := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys/stored./replace", tsigReplaceBody(original.Key, 0x61)), http.StatusOK)
	second, _ := f.s.opts.Keyrings.Capture().Keys().Identity("stored.")
	if replaced.Key.UID != original.Key.UID || replaced.Key.ResourceVersion == original.Key.ResourceVersion || first.CredentialVersion == second.CredentialVersion {
		t.Fatal("same bytes revived original epoch")
	}
	stale := tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys/stored./revoke", map[string]any{"uid": original.Key.UID, "resourceVersion": original.Key.ResourceVersion})
	if stale.Code != http.StatusConflict {
		t.Fatal("stale original binding was silently refreshed")
	}
	resealed := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys/stored./reseal", map[string]any{"uid": replaced.Key.UID, "resourceVersion": replaced.Key.ResourceVersion}), http.StatusOK)
	sealID, _ := f.s.opts.Keyrings.Capture().Keys().Identity("stored.")
	if sealID != second || resealed.Key.ResourceVersion == replaced.Key.ResourceVersion {
		t.Fatal("reseal changed epoch or omitted durable envelope effect")
	}
	revoked := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys/stored./revoke", map[string]any{"uid": resealed.Key.UID, "resourceVersion": resealed.Key.ResourceVersion}), http.StatusOK)
	if !revoked.Removed || revoked.Key.ResourceVersion != resealed.Key.ResourceVersion {
		t.Fatal("revoke receipt fabricated a surviving version")
	}
	if _, ok := f.s.opts.Keyrings.Capture().Keys().Identity("stored."); ok {
		t.Fatal("acknowledged revoke retained credential")
	}
	recreated := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys", tsigCreateBody("stored.", 0x61)), http.StatusCreated)
	final, _ := f.s.opts.Keyrings.Capture().Keys().Identity("stored.")
	if recreated.Key.UID == original.Key.UID || final.CredentialVersion == first.CredentialVersion || final.CredentialVersion == second.CredentialVersion {
		t.Fatal("delete/recreate revived original owner")
	}
	file := tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys/updates./revoke", map[string]any{"uid": "invented", "resourceVersion": "1"})
	if file.Code != http.StatusConflict {
		t.Fatal("file source mutation accepted")
	}
	list := tsigRouteRequest(t.Context(), t, server, f.token, http.MethodGet, "/tsigkeys", nil)
	var metadata api.TSIGKeyList
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &metadata) != nil || len(metadata.Items) != 2 {
		t.Fatal("original metadata list missing file/store source")
	}
}

func TestServingTSIGActualRouteHeldSourceAndUnavailableCoordinator(t *testing.T) {
	f := newTSIGCommandFixture(t)
	captured, release := make(chan struct{}), make(chan struct{})
	var hold sync.Once
	server := tsigCommandHTTP(t, f.servingCommandFixture, func(r *http.Request) {
		if r.URL.Path == "/api/v1/tsigkeys/stored./reseal" {
			hold.Do(func() {
				close(captured)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
		}
	})
	first := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys", tsigCreateBody("stored.", 0x62)), http.StatusCreated)
	originalRing := f.s.opts.Keyrings.Capture()
	acquired := f.source.captureAcquisition()
	if acquired == nil {
		t.Fatal("capture original acquisition source")
	}
	f.stopSource()
	heldDone := make(chan *httptest.ResponseRecorder, 1)
	heldCtx, cancelHeld := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancelHeld()
	heldJoined := false
	defer func() {
		cancelHeld()
		select {
		case <-release:
		default:
			close(release)
		}
		if !heldJoined {
			select {
			case <-heldDone:
			case <-time.After(time.Second):
				t.Error("held original route did not join cleanup")
			}
		}
	}()
	go func() {
		heldDone <- tsigRouteRequest(heldCtx, t, server, f.token, http.MethodPost, "/tsigkeys/stored./reseal", map[string]any{"uid": first.Key.UID, "resourceVersion": first.Key.ResourceVersion})
	}()
	select {
	case <-captured:
	case <-heldCtx.Done():
		t.Fatal("original management route did not capture before source change")
	}
	commandCtx, cancelCommand := context.WithTimeout(t.Context(), 3*time.Second)
	second := tsigReadReceipt(t, tsigRouteRequest(commandCtx, t, server, f.token, http.MethodPost, "/tsigkeys/stored./replace", tsigReplaceBody(first.Key, 0x63)), http.StatusAccepted)
	cancelCommand()
	if !originalRing.Current() || !acquired.Current() || second.Key.ResourceVersion == first.Key.ResourceVersion {
		t.Fatal("fixture did not retain old memory source across real committed route")
	}
	cache, publication := false, false
	if err := acquired.WithCurrent(t.Context(), func() error { cache = true; return nil }, func() error { publication = true; return nil }); !errors.Is(err, tsigkeys.ErrSource) || cache || publication {
		t.Fatal("old ring certified changed source during coordinator absence")
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal("read actual committed image")
	}
	stale, captureErr := f.s.captureHost(t.Context(), image)
	if stale.Release != nil {
		stale.Release()
	}
	if captureErr == nil {
		t.Fatal("failed source preparation exposed old complete keys")
	}
	close(release)
	select {
	case result := <-heldDone:
		heldJoined = true
		if result.Code != http.StatusServiceUnavailable || strings.Contains(result.Body.String(), `"committed":true`) {
			t.Fatal("held old route gained successor source authority")
		}
	case <-heldCtx.Done():
		t.Fatal("held original route did not finish")
	}
	row := tsigReadPrivateRow(t, f.s.opts.Store, "stored.")
	if row.ResourceVersion != second.Key.ResourceVersion {
		t.Fatal("refused original route changed committed source")
	}
	f.startSource()
	awaitServingRuntime(t, f.s, func() bool {
		current := f.s.opts.Keyrings.Capture().Prepared()
		return f.s.publisher.Status().ProtectedReady && current != nil && current.SourceIdentity() != originalRing.Prepared().SourceIdentity()
	})
	fresh := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, f.token, http.MethodPost, "/tsigkeys/stored./reseal", map[string]any{"uid": second.Key.UID, "resourceVersion": second.Key.ResourceVersion}), http.StatusOK)
	if fresh.Key.ResourceVersion == second.Key.ResourceVersion {
		t.Fatal("fresh independent intent did not commit")
	}
}

func TestServingTSIGPostcommitQuietOriginalExpiryAndSafeUnknownError(t *testing.T) {
	f := newTSIGCommandFixture(t)
	server := tsigCommandHTTP(t, f.servingCommandFixture, nil)
	f.stopSource()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	result := make(chan *httptest.ResponseRecorder, 1)
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-result:
			case <-time.After(time.Second):
				t.Error("original expiry route did not join cleanup")
			}
		}
	}()
	go func() {
		result <- tsigRouteRequest(ctx, t, server, f.token, http.MethodPost, "/tsigkeys", tsigCreateBody("expiry.", 0x64))
	}()
	for {
		changed := f.s.opts.Store.GenerationChanges()
		var row kinds.TSIGKey
		if f.s.opts.Store.Get(ctx, store.Key{Kind: kinds.KindTSIGKey, Name: kinds.ObjectName("expiry.")}, &row) == nil {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("single route did not commit before original expiry")
		}
	}
	// The original detached credential expires after the actual row commit
	// with source acquisition paused. Other real workers can still emit events;
	// this component fixture does not establish ticker-only expiry attribution.
	f.offset.Store(int64(time.Hour))
	expiryObserved := time.Now()
	select {
	case response := <-result:
		joined = true
		receipt := tsigReadReceipt(t, response, http.StatusAccepted)
		if receipt.Published || time.Since(expiryObserved) > time.Second {
			t.Fatal("quiet expiry renewed original lifetime")
		}
	case <-ctx.Done():
		t.Fatal("quiet original expiry was not observed within original budget")
	}
	if tsigReadPrivateRow(t, f.s.opts.Store, "expiry.").UID == "" {
		t.Fatal("postcommit expiry implied rollback")
	}
	safe := tsigKeyAPIError(errors.New("private SQL completion and secret detail"))
	if strings.Contains(safe.Error(), "private SQL") || strings.Contains(safe.Error(), "retry") || strings.Contains(safe.Error(), "rolled back") {
		t.Fatal("unknown outcome disclosed detail or promised rollback/replay")
	}
}

func TestServingTSIGPrincipalBindingRejectsRebinding(t *testing.T) {
	original := auth.Principal{Kind: auth.KindUser, ID: "same-name", Name: "display", Provider: "local", AccountUID: "original", AuthMethod: auth.MethodToken, TokenID: "original-token", CredentialIssuedAt: time.Now().UTC()}
	rebound := original
	rebound.AccountUID = "successor"
	if tsigPrincipalBinding(original) == tsigPrincipalBinding(rebound) {
		t.Fatal("same display identity adopted successor account")
	}
	rebound = original
	rebound.External = &auth.ExternalProof{}
	if tsigPrincipalBinding(original) == tsigPrincipalBinding(rebound) {
		t.Fatal("absent external identity equaled attached evidence")
	}
}

func TestServingTSIGInitializedFollowerRestartAndOriginalPromotion(t *testing.T) {
	primary := newTSIGCommandFixture(t)
	primaryHTTP := tsigCommandHTTP(t, primary.servingCommandFixture, nil)
	original := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, primaryHTTP, primary.token, http.MethodPost, "/tsigkeys", tsigCreateBody("replica.", 0x65)), http.StatusCreated)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal("prepare replica scheme")
	}
	path := filepath.Join(t.TempDir(), "follower.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: primary.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "tsig-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal("open actual follower state")
		}
		return st
	}
	follower := open()
	setup, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if follower.Enroll(setup, primary.s.opts.Store.Authority().Term) != nil {
		t.Fatal("enroll actual follower")
	}
	if _, err := follower.BindAnchorLifecycle(setup); err != nil {
		t.Fatal("bind actual follower anchor lifecycle")
	}
	setup, err = follower.CaptureAnchorInstall(setup)
	if err != nil {
		t.Fatal("capture original follower before source image")
	}
	image, err := primary.s.opts.Store.ReadGeneration(setup)
	if err != nil {
		t.Fatal("read actual initialized private image")
	}
	validated, err := follower.ValidateImage(setup, *image)
	if err != nil {
		t.Fatal("validate actual complete private replica")
	}
	installed, err := follower.InstallReplica(setup, validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil || follower.Close() != nil {
		t.Fatal("install and close original replica")
	}
	follower = open()
	t.Cleanup(func() { _ = follower.Close() })
	rings, err := servingbuild.NewKeyringOwner(primary.source.baseline)
	if err != nil {
		t.Fatal("prepare follower original file baseline")
	}
	t.Cleanup(func() { _ = rings.Close(context.Background()) })
	source, err := newTSIGRuntime(follower, primary.s.opts.AuthKeys, rings, primary.source.baseline)
	if err != nil || source.prepare(t.Context()) != nil {
		t.Fatal("reopen stored source with original encryption owner")
	}
	opts := primary.s.opts
	opts.Store, opts.Keyrings, opts.TSIG = follower, rings, source
	opts.Transferred = servingbuild.NewTransferredRuntime(nil, nil, nil)
	runtime, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal("prepare actual follower complete runtime")
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runServingFixture(t, runtime)
	awaitServingRuntime(t, runtime, func() bool { return runtime.publisher.Status().ProtectedReady })
	adapter, err := servinghttp.New(servinghttp.Options{Publisher: runtime.publisher, Now: runtime.opts.Now})
	if err != nil {
		t.Fatal("prepare follower HTTP cut")
	}
	server := tsigCommandHTTP(t, &servingCommandFixture{s: runtime, http: adapter}, nil)
	response := tsigRouteRequest(t.Context(), t, server, primary.token, http.MethodGet, "/tsigkeys", nil)
	if response.Code != http.StatusOK {
		t.Fatal("initialized follower did not serve original metadata")
	}
	held, err := runtime.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal("retain original follower publication")
	}
	defer held.Release()
	key, ok := rings.Capture().Keys().Identity("replica.")
	initialKey, _ := primary.s.opts.Keyrings.Capture().Keys().Identity("replica.")
	if !ok || key != initialKey || !held.Bundle().Identity().Base.Same(installed) {
		t.Fatal("follower restart changed original stored credential/image")
	}
	refused := tsigRouteRequest(t.Context(), t, server, primary.token, http.MethodPost, "/tsigkeys/replica./replace", tsigReplaceBody(original.Key, 0x66))
	after, err := follower.ReadGeneration(t.Context())
	if refused.Code != http.StatusServiceUnavailable || err != nil || !after.Generation().Same(installed) {
		t.Fatal("follower management minted writer authority")
	}
	promoted, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "tsig-management-promotion"})
	if err != nil {
		t.Fatal("perform original supported promotion")
	}
	writerCtx, cancelWriter := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- follower.Elector("tsig-promoted-writer").Run(writerCtx, func(ctx context.Context) {
			if err := runtime.RunWriter(ctx); err != nil && ctx.Err() == nil && runtime.life.Err() == nil {
				t.Error("promoted original writer failed")
			}
		}, nil)
	}()
	t.Cleanup(func() {
		cancelWriter()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("promoted original writer did not join cleanly")
			}
		case <-time.After(10 * time.Second):
			t.Error("promoted writer did not join")
		}
	})
	awaitServingRuntime(t, runtime, func() bool {
		return runtime.primary.driver.Status().Ready && runtime.publisher.Status().ProtectedReady && runtime.publisher.Status().Served.Base.Term.Same(promoted.Term)
	})
	replacement := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, server, primary.token, http.MethodPost, "/tsigkeys/replica./replace", tsigReplaceBody(original.Key, 0x66)), http.StatusOK)
	if replacement.Key.UID != original.Key.UID || replacement.Key.ResourceVersion == original.Key.ResourceVersion || !held.Bundle().Identity().Base.Same(installed) {
		t.Fatal("promotion rewrote held source or refreshed original binding")
	}
	sourceCut, _ := tsigkeys.FromImage(t.Context(), image)
	oldRaw, _ := held.Bundle().Component("runtime")
	if !oldRaw.(*servingbuild.RuntimeOwner).MatchesTSIGTarget(sourceCut.Identity(), sourceCutEncryption(t, primary.source)) {
		t.Fatal("held follower publication adopted promoted key source")
	}
}

func sourceCutEncryption(t *testing.T, source *tsigRuntime) string {
	t.Helper()
	prepared := source.rings.Capture().Prepared()
	if prepared == nil {
		t.Fatal("original Prepared encryption owner absent")
	}
	return prepared.EncryptionIdentity()
}
