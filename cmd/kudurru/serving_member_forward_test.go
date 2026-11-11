package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type memberForwardHTTPFixture struct {
	primary  *servingCommandFixture
	follower *servingRuntime
	puller   *clusterreplica.Coordinator
	live     *httptest.Server
	hold     atomic.Pointer[memberForwardBodyHold]
	// api is the follower's mounted API server; cache is the response cache
	// its node-local flush command owns.
	api   *api.Server
	cache *cache.Cache
	// scheduledPull runs the follower's own pull worker, as runServe does.
	scheduledPull bool
}

// memberForwardScheduledPull starts the follower's scheduled pull worker and
// lets a forwarded login install its session. The default fixture pulls only
// when a test asks, so that a receipt can be checked before any installation.
func memberForwardScheduledPull(f *memberForwardHTTPFixture) { f.scheduledPull = true }

type memberForwardBodyHold struct {
	entered, release chan struct{}
	once             sync.Once
}
type memberForwardHeldBody struct {
	io.ReadCloser
	hold *memberForwardBodyHold
}

func (b *memberForwardHeldBody) Read(p []byte) (int, error) {
	b.hold.once.Do(func() { close(b.hold.entered) })
	<-b.hold.release
	return b.ReadCloser.Read(p)
}

// First enrollment/image setup is explicit fixture administration. Public
// forwarding uses both complete runtimes and the production member listener;
// later follower observation uses the actual Coordinator pull, started explicitly
// so the 202 receipt can be checked before any local image installation.
func newMemberForwardHTTPFixture(t *testing.T, http2 bool, options ...func(*memberForwardHTTPFixture)) *memberForwardHTTPFixture {
	t.Helper()
	f := new(memberForwardHTTPFixture)
	for _, option := range options {
		option(f)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	dir := t.TempDir()
	var mirror *sqlite.Store
	f.primary = newServingCommandFixture(t, func(s *servingRuntime) {
		s.opts.Config.Node.DataDir = t.TempDir()
		scheme, err := runtimekinds.NewScheme()
		if err != nil {
			t.Fatal(err)
		}
		mirror, err = sqlite.Open(filepath.Join(dir, sqlite.FileName), sqlite.Options{Scheme: scheme, Validator: s.opts.Validator,
			InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mirror.Close() })
		enrollServingMemberFixtureAt(t, s, listener.Addr().String(), mirror.Authority().MemberID,
			filepath.Join(s.opts.Config.Node.DataDir, memberIdentityFile), filepath.Join(dir, memberIdentityFile))
	})
	p := f.primary.s
	// Forwarded object and zone commands run the primary's own public command
	// bodies, as runServe mounts them.
	primaryAPI, err := api.New(api.Options{Store: p.opts.Store, Validator: p.opts.Validator, Config: p.opts.Config, CanonicalSettings: p,
		ObjectCommand: p.ObjectCommand, ZoneCommand: p.ZoneCommand, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(primaryAPI.Close)
	p.objectAPI.Store(primaryAPI)
	if err := mirror.Enroll(t.Context(), p.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	install := captureServingReplica(t, mirror)
	image, err := p.opts.Store.ReadGeneration(install)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := mirror.ValidateImage(install, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.InstallReplica(install, validated, replication.InstallOptions{ExpectedTerm: image.Term}); err != nil {
		t.Fatal(err)
	}
	credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{51}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := p.prepareMemberListener(t.Context(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: credentials, Enrolled: enrolled})
	if err != nil {
		t.Fatal(err)
	}
	serverContext, stop := context.WithCancel(t.Context())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverContext, listener) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("member server did not join")
		}
	})
	// Real nodes have independent owner locks. Reusing p.opts here shares its
	// exclusive auth-key owner across the two ends of the member exchange.
	receiverBase := newServingRuntimeFixture(t)
	opts := receiverBase.opts
	opts.Now = p.opts.Now
	config := *opts.Config
	config.Node.Name, config.Node.DataDir = "receiver", dir
	opts.Config, opts.Store = &config, mirror
	f.follower, err = newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.follower.Close(context.Background()) })
	_, f.puller, f.follower.memberForward, err = f.follower.prepareMemberServices(t.Context(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	if f.scheduledPull {
		f.follower.member = f.puller
	}
	runServingFixture(t, f.follower)
	awaitServingRuntime(t, f.follower, func() bool { return f.follower.publisher.Status().ProtectedReady })
	h, err := newServingHTTP(servingHTTPOptions{Runtime: f.follower, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	// The follower's role source, authority membership and node-local cache
	// command are mounted as runServe mounts them.
	f.cache = servingCacheEntries()
	flush, err := newServingCacheCommand(f.follower, f.cache)
	if err != nil {
		t.Fatal(err)
	}
	apiServer, err := api.New(api.Options{Store: mirror, Validator: opts.Validator, Config: opts.Config, CanonicalSettings: f.follower,
		BlockingCommand: f.follower.BlockingCommand, BlockingReads: servingbuild.NewBlockingReads(),
		ObjectCommand: f.follower.ObjectCommand, ObjectForward: f.follower.ObjectForward,
		ZoneCommand: f.follower.ZoneCommand, ZoneForward: f.follower.ZoneForward, SettingsForward: f.follower.SettingsForward,
		ClusterRole: f.follower.ClusterRole, CacheFlushCommand: flush, NodeName: opts.Config.Node.Name,
		Membership:   cluster.Authority{SelfName: opts.Config.Node.Name, Elector: mirror.Elector(cluster.ElectionController)},
		Authenticate: h.Middleware, AuthHandler: h.Handler(), Authorizer: h.Authorizer(), Roles: h.Roles, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f.api = apiServer
	f.live = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if held := f.hold.Swap(nil); held != nil {
			r.Body = &memberForwardHeldBody{r.Body, held}
		}
		apiServer.ServeHTTP(w, r)
	}))
	f.live.EnableHTTP2 = http2
	f.live.StartTLS()
	t.Cleanup(f.live.Close)
	return f
}

func (f *memberForwardHTTPFixture) call(ctx context.Context, body, token string) (*http.Response, []byte, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, f.live.URL+api.BasePath+"/dns/blocking", strings.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := f.live.Client().Do(r)
	if err != nil {
		return response, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	return response, data, err
}

func TestRootMemberForwardBlockingActualHTTPAndLaterPublication(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "http1"
		if http2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			f := newMemberForwardHTTPFixture(t, http2)
			before := f.follower.opts.Store.Authority()
			response, body, err := f.call(t.Context(), `{"blocking":false,"timer":60}`, f.primary.token)
			if err != nil {
				t.Fatal("actual follower exchange", err)
			}
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("forward status=%d body=%s", response.StatusCode, body)
			}
			if response.ProtoMajor != map[bool]int{false: 1, true: 2}[http2] {
				t.Fatal("wrong actual HTTP protocol")
			}
			var receipt blockingReceipt
			if json.Unmarshal(body, &receipt) != nil || !receipt.Committed || !receipt.Disabled || receipt.Publication != "pending" || receipt.DisabledUntil == nil || !receipt.DisabledUntil.Equal(f.primary.now.Add(time.Minute)) {
				t.Fatal("forwarded receipt lost original absolute intent or claimed publication")
			}
			if before.InstallSequence != f.follower.opts.Store.Authority().InstallSequence {
				t.Fatal("forwarded command installed its own response")
			}
			var settings public.Kudurru
			if err := f.primary.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil || settings.ResourceVersion != receipt.ResourceVersion || !settings.Status.Blocking.Disabled {
				t.Fatal("primary commit missing", err)
			}
			entries, err := f.primary.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Kudurru", Name: "default"})
			attributed := 0
			for _, entry := range entries {
				if entry.RequestPath == clusterreplica.PathWrite && entry.RequestID != "" && entry.Actor != "" && entry.Authz != "" {
					attributed++
				}
			}
			if err != nil || attributed != 1 {
				t.Fatal("missing same-command attributed primary audit", attributed, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := f.puller.Pull(ctx); err != nil {
				t.Fatal("actual subsequent pull", err)
			}
			awaitServingRuntime(t, f.follower, func() bool {
				return f.follower.publisher.Status().ProtectedReady && f.follower.publisher.Status().Served.Base.Same(f.follower.opts.Store.Authority().Installed)
			})
			if err := f.follower.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil || settings.ResourceVersion != receipt.ResourceVersion {
				t.Fatal("later local replica differs", err)
			}
		})
	}
}

func TestRootMemberForwardOriginalBodyCutRefusesSuccessor(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, true)
	held := &memberForwardBodyHold{entered: make(chan struct{}), release: make(chan struct{})}
	f.hold.Store(held)
	var once sync.Once
	release := func() { once.Do(func() { close(held.release) }) }
	defer release()
	type result struct {
		response *http.Response
		body     []byte
		err      error
	}
	finished := make(chan result, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() { r, b, e := f.call(ctx, `{"blocking":false}`, f.primary.token); finished <- result{r, b, e} }()
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("request did not reach original body read")
	}
	w, err := f.primary.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.primary.s.opts.Store.Command(sqlite.WithWriterToken(ctx, w), w.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "forward-interposition", Data: []byte(`{"changed":true}`)})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.puller.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case got := <-finished:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.response.StatusCode < 400 {
			t.Fatal("late body adopted successor and forwarded", got.response.StatusCode)
		}
	case <-ctx.Done():
		t.Fatal("original request did not join")
	}
	var settings public.Kudurru
	if err := f.primary.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil || settings.Status.Blocking.Disabled {
		t.Fatal("refused original body mutated primary", err)
	}
}

func TestRootMemberForwardRejectsInvalidProofAndBodyWithoutMutation(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false)
	before := roleBindingImage(t, f.primary.s.opts.Store)
	for _, body := range []string{`{}`, `null`, `{"blocking":false,"blocking":true}`, `{"blocking":false,"extra":true}`, `{"blocking":false} {}`, `{"blocking":false,"timer":-1}`} {
		r, _, err := f.call(t.Context(), body, f.primary.token)
		if err != nil || r.StatusCode != http.StatusBadRequest {
			t.Fatal("malformed intent not refused", err)
		}
	}
	r, _, err := f.call(t.Context(), `{"blocking":false}`, "invalid-original-proof")
	if err != nil || r.StatusCode != http.StatusUnauthorized {
		t.Fatal("invalid raw proof not refused", err)
	}
	roleBindingUnchanged(t, f.primary.s.opts.Store, before)
}

// A forwarded blocking switch answers as the other forwarded domains do
// (D368): a follower whose settings view is older than the primary's gets the
// primary's retained 409 instead of an unexplained 503, nothing is changed,
// and the same switch succeeds once the follower has pulled. An exchange with
// a primary that cannot commit says that the change was not sent or that its
// outcome is unknown, never the generic authentication text.
func TestRootMemberForwardBlockingStaleViewAndFailedExchange(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false)
	primaryStore := f.primary.s.opts.Store
	settings := func() public.Kudurru {
		t.Helper()
		var out public.Kudurru
		if err := primaryStore.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// The primary's settings change after the follower's last pull.
	writer, err := primaryStore.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := primaryStore.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		var current public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &current); err != nil {
			return err
		}
		current.Status.Blocking = public.BlockingStatus{Disabled: true}
		return tx.UpdateStatus(ctx, &current)
	}); err != nil {
		t.Fatal(err)
	}
	changed := settings()
	stale := f.request(t, nil, http.MethodPost, "/dns/blocking", f.primary.token, map[string]any{"blocking": true})
	if stale.status != http.StatusConflict || stale.Error.Key != api.KeyConflict || !strings.Contains(stale.Error.Message, "follower's view") ||
		stale.header.Get("Retry-After") != "" {
		t.Fatalf("stale follower view: %d %s", stale.status, stale.raw)
	}
	if after := settings(); after.ResourceVersion != changed.ResourceVersion || !after.Status.Blocking.Disabled {
		t.Fatal("a refused blocking switch changed the primary's settings")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := f.puller.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.follower, func() bool {
		return f.follower.publisher.Status().ProtectedReady && f.follower.publisher.Status().Served.Base.Same(f.follower.opts.Store.Authority().Installed)
	})
	if fresh := f.request(t, nil, http.MethodPost, "/dns/blocking", f.primary.token, map[string]any{"blocking": true}); fresh.status != http.StatusAccepted {
		t.Fatalf("blocking switch after the pull: %d %s", fresh.status, fresh.raw)
	}
	if after := settings(); after.Status.Blocking.Disabled {
		t.Fatal("the switch after the pull was not committed")
	}
	// The primary's writer stops: the exchange cannot commit.
	f.primary.stop()
	lost := f.request(t, nil, http.MethodPost, "/dns/blocking", f.primary.token, map[string]any{"blocking": false})
	if lost.status != http.StatusServiceUnavailable || lost.Error.Key != api.KeyUnavailable && lost.Error.Key != "completion_unknown" ||
		strings.Contains(lost.Error.Message, "authentication operation unavailable") || lost.Error.Message == "" {
		t.Fatalf("blocking switch while the primary cannot commit: %d %s", lost.status, lost.raw)
	}
}
