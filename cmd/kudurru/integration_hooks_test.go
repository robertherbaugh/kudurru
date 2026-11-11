package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hookPost is one POST /events the peer received.
type hookPost struct {
	id, signature string
	body          []byte
	at            time.Time
	topic         string
	payload       map[string]any
}

// hookPeer is a TLS Tier-B peer that advertises only hooks (D379).
type hookPeer struct {
	server *httptest.Server
	mu     sync.Mutex
	posts  []hookPost
	answer func(hookPost) int
}

func newHookPeer(t *testing.T) *hookPeer {
	t.Helper()
	p := &hookPeer{}
	p.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only-private-credential" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/provider/":
			w.Header().Set("Content-Type", webhookMountMedia)
			_ = json.NewEncoder(w).Encode(map[string]any{"filters": []string{}, "capabilities": []string{"hooks"}, "manifest": integration.Manifest{Type: "fixture", DisplayName: "Fixture", Version: "1", IOClass: "local_push", Capabilities: []integration.Capability{integration.CapHook}, ConfigSchema: json.RawMessage(`{"type":"object"}`)}})
		case "/provider/events":
			b, _ := io.ReadAll(r.Body)
			post := hookPost{id: r.Header.Get("X-Kudurru-Delivery"), signature: r.Header.Get("X-Kudurru-Signature"), body: b, at: time.Now()}
			var event struct {
				Topic   string         `json:"topic"`
				Payload map[string]any `json:"payload"`
			}
			_ = json.Unmarshal(b, &event)
			post.topic, post.payload = event.Topic, event.Payload
			p.mu.Lock()
			p.posts = append(p.posts, post)
			answer := p.answer
			p.mu.Unlock()
			code := http.StatusNoContent
			if answer != nil {
				code = answer(post)
			}
			w.WriteHeader(code)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *hookPeer) setAnswer(fn func(hookPost) int) {
	p.mu.Lock()
	p.answer = fn
	p.mu.Unlock()
}

// about returns the posts of one topic whose payload names uid.
func (p *hookPeer) about(topic, uid string) []hookPost {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []hookPost
	for _, post := range p.posts {
		if post.topic == topic && post.payload["uid"] == uid {
			out = append(out, post)
		}
	}
	return out
}

type hookFixture struct {
	s          *servingRuntime
	d          *public.Integration
	key        []byte
	keyPath    string
	stopWriter func()
}

func newHookFixture(t *testing.T, p *hookPeer) *hookFixture {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "peer-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	f := &hookFixture{key: []byte(strings.Repeat("hook-signing-key-", 3)), keyPath: filepath.Join(dir, "hmac")}
	if err := os.WriteFile(f.keyPath, f.key, 0600); err != nil {
		t.Fatal(err)
	}
	var path string
	f.s, path = integrationFixture(t, func(o *servingRuntimeOptions) {
		o.Config.Credentials.AllowedFiles = append(o.Config.Credentials.AllowedFiles, ca, f.keyPath)
	}, nil)
	d := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "hooks", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "webhook", Credentials: map[string]public.CredentialReference{"bearerToken": {File: path}, "ca": {File: ca}, "hmacKey": {File: f.keyPath}}, Webhook: &public.WebhookIntegration{Endpoint: p.server.URL + "/provider"}, Resync: metav1.Duration{Duration: 10 * time.Minute}}}
	f.d = integrationApply(t, f.s, d)
	f.stopWriter = integrationRun(t, f.s)
	f.awaitBaseline(t)
	return f
}

// awaitBaseline waits for the running hook instance and its first cursor.
func (f *hookFixture) awaitBaseline(t *testing.T) {
	t.Helper()
	owner := integrationOwner(f.s, f.d)
	awaitServingRuntime(t, f.s, func() bool {
		running := false
		for _, s := range f.s.integrations.host.Statuses() {
			running = running || s.Owner == owner && (s.Phase == "Running" || s.Phase == "Degraded")
		}
		var c kinds.IntegrationEventCursor
		return running && f.s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationEventCursor, Name: kinds.EventCursorName(owner)}, &c) == nil
	})
}

func (f *hookFixture) signed(t *testing.T, post hookPost) {
	t.Helper()
	mac := hmac.New(sha256.New, f.key)
	_, _ = mac.Write(post.body)
	if post.signature != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("event body is not signed with the instance key")
	}
}

func hookRecord(name string) *public.DNSRecordSet {
	return &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: name + ".example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.44"}}}}}
}

func hookApply(t *testing.T, st *sqlite.Store, obj store.Object) store.Object {
	t.Helper()
	out, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func awaitPosts(t *testing.T, s *servingRuntime, p *hookPeer, topic, uid string, n int) []hookPost {
	t.Helper()
	var posts []hookPost
	awaitServingRuntime(t, s, func() bool {
		posts = p.about(topic, uid)
		return len(posts) >= n
	})
	return posts
}

// A committed record set reaches the peer as exactly one signed
// record.changed with its stable ID, and later status writes of the same
// generation do not repeat it.
func TestIntegrationWebhookHooksMountedRecordChange(t *testing.T) {
	p := newHookPeer(t)
	f := newHookFixture(t, p)
	record := hookApply(t, f.s.opts.Store, hookRecord("hooked")).(*public.DNSRecordSet)
	awaitPosts(t, f.s, p, "record.changed", string(record.UID), 1)
	time.Sleep(3 * time.Second) // more ticks; nothing may repeat the event
	posts := p.about("record.changed", string(record.UID))
	if len(posts) != 1 {
		t.Fatal("record change delivered", len(posts), "times")
	}
	f.signed(t, posts[0])
	want := ih.DeliveryID(integrationOwner(f.s, f.d), ih.ProducedEvent{Topic: "record.changed", Subject: string(record.UID), Version: "1"})
	if posts[0].id != want || posts[0].payload["generation"] != float64(1) || posts[0].payload["change"] != "added" {
		t.Fatalf("record event: id=%s payload=%v", posts[0].id, posts[0].payload)
	}
	// The zone's committed serial changed with it: one zone.changed per
	// serial, never a duplicate ID.
	p.mu.Lock()
	seen := map[string]bool{}
	for _, post := range p.posts {
		if seen[post.id] {
			t.Error("an acknowledged event was delivered again", post.topic)
		}
		seen[post.id] = true
	}
	p.mu.Unlock()
	// A deletion is its own event.
	if err := f.s.opts.Store.Delete(t.Context(), store.KeyOf(record), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	posts = awaitPosts(t, f.s, p, "record.changed", string(record.UID), 2)
	if posts[1].payload["change"] != "deleted" {
		t.Fatal("deletion event", posts[1].payload)
	}
}

// A 503 is retried with backoff under the same ID, body and signature.
func TestIntegrationWebhookHooksMountedRetryKeepsDelivery(t *testing.T) {
	p := newHookPeer(t)
	failed := false
	p.setAnswer(func(post hookPost) int {
		if post.topic == "record.changed" && !failed {
			failed = true
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	f := newHookFixture(t, p)
	record := hookApply(t, f.s.opts.Store, hookRecord("retried")).(*public.DNSRecordSet)
	posts := awaitPosts(t, f.s, p, "record.changed", string(record.UID), 2)
	if posts[0].id != posts[1].id || string(posts[0].body) != string(posts[1].body) || posts[0].signature != posts[1].signature {
		t.Fatal("the retry changed the delivery")
	}
	if gap := posts[1].at.Sub(posts[0].at); gap < time.Second {
		t.Fatal("retried without backoff", gap)
	}
	f.signed(t, posts[1])
	time.Sleep(3 * time.Second)
	if n := len(p.about("record.changed", string(record.UID))); n != 2 {
		t.Fatal("acknowledged event delivered again", n)
	}
}

// A 4xx drops the event, raises Degraded and publishes it.
func TestIntegrationWebhookHooksMountedRefusalDeadLetters(t *testing.T) {
	p := newHookPeer(t)
	p.setAnswer(func(post hookPost) int {
		if post.topic == "record.changed" {
			return http.StatusBadRequest
		}
		return http.StatusNoContent
	})
	f := newHookFixture(t, p)
	record := hookApply(t, f.s.opts.Store, hookRecord("refused")).(*public.DNSRecordSet)
	posts := awaitPosts(t, f.s, p, "record.changed", string(record.UID), 1)
	owner := integrationOwner(f.s, f.d)
	awaitServingRuntime(t, f.s, func() bool {
		var d public.Integration
		if f.s.opts.Store.Get(t.Context(), store.KeyOf(f.d), &d) != nil {
			return false
		}
		ready := meta.FindStatusCondition(d.Status.Conditions, "Ready")
		return ready != nil && ready.Status == metav1.ConditionFalse && ready.Reason == ih.ReasonEventDeadLettered
	})
	var row kinds.IntegrationDelivery
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationDelivery, Name: kinds.DeliveryName(owner, posts[0].id)}, &row); err != nil || !row.Spec.DeadLetter || row.Spec.ErrorClass != "permanent" {
		t.Fatal("refused event not dead-lettered", err, row.Spec)
	}
	time.Sleep(3 * time.Second)
	if n := len(p.about("record.changed", string(record.UID))); n != 1 {
		t.Fatal("dead letter retried", n)
	}
	// The instance's own Degraded status is an integration.error event.
	awaitPosts(t, f.s, p, "integration.error", string(f.d.UID), 1)
}

// restartHooks closes the runtime, runs between on the still open store,
// and starts a new runtime over the same database.
func (f *hookFixture) restart(t *testing.T, between func(*sqlite.Store)) {
	t.Helper()
	old := f.s
	f.stopWriter()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if between != nil {
		between(old.opts.Store)
	}
	path := old.opts.Store.Path()
	if err := old.opts.Store.Close(); err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: old.opts.Validator, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	opts := old.opts
	opts.Store = st
	next, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := next.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.s = next
	f.stopWriter = integrationRun(t, next)
	f.awaitBaseline(t)
}

// A change committed while the runtime is stopped is delivered once after
// the restart, from the durable cursor.
func TestIntegrationWebhookHooksMountedRestartDeliversOnce(t *testing.T) {
	p := newHookPeer(t)
	f := newHookFixture(t, p)
	var record *public.DNSRecordSet
	f.restart(t, func(st *sqlite.Store) {
		record = hookApply(t, st, hookRecord("offline")).(*public.DNSRecordSet)
	})
	awaitPosts(t, f.s, p, "record.changed", string(record.UID), 1)
	time.Sleep(3 * time.Second)
	if n := len(p.about("record.changed", string(record.UID))); n != 1 {
		t.Fatal("change committed while stopped delivered", n, "times")
	}
}

// A cursor the feed cannot resume (here, newer than the store) degrades to
// one zone.changed per zone and a fresh baseline, never one event per object.
func TestIntegrationWebhookHooksMountedStaleCursorResyncsZones(t *testing.T) {
	p := newHookPeer(t)
	f := newHookFixture(t, p)
	record := hookApply(t, f.s.opts.Store, hookRecord("before-reset")).(*public.DNSRecordSet)
	first := awaitPosts(t, f.s, p, "record.changed", string(record.UID), 1)
	owner := integrationOwner(f.s, f.d)
	// Restart only after the acknowledgment is durable: a delivery the
	// peer answered but the runtime did not record is legitimately sent
	// again after a restart (at-least-once), which is not what this checks.
	awaitServingRuntime(t, f.s, func() bool {
		var row kinds.IntegrationDelivery
		return f.s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationDelivery, Name: kinds.DeliveryName(owner, first[0].id)}, &row) == nil && row.Spec.Acknowledged
	})
	var zone public.DNSZone
	f.restart(t, func(st *sqlite.Store) {
		key := store.Key{Kind: kinds.KindIntegrationEventCursor, Name: kinds.EventCursorName(owner)}
		var c kinds.IntegrationEventCursor
		if err := st.Get(t.Context(), key, &c); err != nil {
			t.Fatal(err)
		}
		for _, kind := range integrationEventKinds {
			c.Spec.Positions[string(kind)] = "999999999"
		}
		if _, err := st.Apply(t.Context(), &c, store.ApplyOptions{Manager: "integration-ledger", ExpectedResourceVersion: c.ResourceVersion}); err != nil {
			t.Fatal(err)
		}
		if err := st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "home"}, &zone); err != nil {
			t.Fatal(err)
		}
	})
	awaitServingRuntime(t, f.s, func() bool {
		for _, post := range p.about("zone.changed", string(zone.UID)) {
			if post.payload["change"] == "resync" {
				return true
			}
		}
		return false
	})
	// The fallback took a fresh baseline: the cursor resumes again.
	awaitServingRuntime(t, f.s, func() bool {
		var c kinds.IntegrationEventCursor
		if f.s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationEventCursor, Name: kinds.EventCursorName(owner)}, &c) != nil {
			return false
		}
		_, resync := c.Spec.Positions[positionResync]
		return !resync && c.Spec.Positions["DNSZone"] != "999999999"
	})
	if n := len(p.about("record.changed", string(record.UID))); n != 1 {
		t.Fatal("the fallback replayed objects", n)
	}
	next := hookApply(t, f.s.opts.Store, hookRecord("after-reset")).(*public.DNSRecordSet)
	awaitPosts(t, f.s, p, "record.changed", string(next.UID), 1)
}

// Rotating the signing key replaces the instance; a pending event is then
// signed with the new key.
func TestIntegrationWebhookHooksMountedKeyRotationResigns(t *testing.T) {
	p := newHookPeer(t)
	var mu sync.Mutex
	accept := false
	p.setAnswer(func(post hookPost) int {
		mu.Lock()
		defer mu.Unlock()
		if post.topic == "record.changed" && !accept {
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	f := newHookFixture(t, p)
	record := hookApply(t, f.s.opts.Store, hookRecord("rotated")).(*public.DNSRecordSet)
	awaitPosts(t, f.s, p, "record.changed", string(record.UID), 1)
	f.key = []byte(strings.Repeat("rotated-signing-key-", 3))
	if err := os.WriteFile(f.keyPath, f.key, 0600); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	accept = true
	mu.Unlock()
	f.s.integrations.host.Wake()
	var last hookPost
	awaitServingRuntime(t, f.s, func() bool {
		posts := p.about("record.changed", string(record.UID))
		last = posts[len(posts)-1]
		mac := hmac.New(sha256.New, f.key)
		_, _ = mac.Write(last.body)
		return last.signature == "sha256="+hex.EncodeToString(mac.Sum(nil))
	})
	first := p.about("record.changed", string(record.UID))[0]
	if first.id != last.id || string(first.body) != string(last.body) {
		t.Fatal("rotation changed the pending delivery")
	}
}

// A promoted replica drains the rows of the installed image: a pending event
// is delivered by the successor under its original ID, an acknowledged one is
// never sent again, and the new term is a cluster.leader_changed event.
func TestIntegrationWebhookHooksMountedPromotionDrainsInstalledRows(t *testing.T) {
	p := newHookPeer(t)
	var mu sync.Mutex
	refuse := map[string]bool{}
	p.setAnswer(func(post hookPost) int {
		mu.Lock()
		defer mu.Unlock()
		if uid, _ := post.payload["uid"].(string); post.topic == "record.changed" && refuse[uid] {
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	f := newHookFixture(t, p)
	primary := f.s
	acked := hookApply(t, primary.opts.Store, hookRecord("acked")).(*public.DNSRecordSet)
	ackedPosts := awaitPosts(t, primary, p, "record.changed", string(acked.UID), 1)
	// The image below must carry the durable acknowledgment.
	awaitServingRuntime(t, primary, func() bool {
		var row kinds.IntegrationDelivery
		return primary.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationDelivery, Name: kinds.DeliveryName(integrationOwner(primary, f.d), ackedPosts[0].id)}, &row) == nil && row.Spec.Acknowledged
	})
	pending := hookRecord("pending")
	pending.UID = "pending-record-uid"
	mu.Lock()
	refuse[string(pending.UID)] = true
	mu.Unlock()
	pending = hookApply(t, primary.opts.Store, pending).(*public.DNSRecordSet)
	refused := awaitPosts(t, primary, p, "record.changed", string(pending.UID), 1)
	owner := integrationOwner(primary, f.d)
	var row kinds.IntegrationDelivery
	if err := primary.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationDelivery, Name: kinds.DeliveryName(owner, refused[0].id)}, &row); err != nil || row.Spec.Acknowledged {
		t.Fatal("pending row", err, row.Spec)
	}

	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "follower.db"), sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "hook-successor", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Enroll(t.Context(), primary.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installCtx := captureServingReplica(t, st)
	image, err := primary.opts.Store.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := st.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := st.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	opts := primary.opts
	opts.Store = st
	follower, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := follower.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	runServingFixture(t, follower)
	promoted, err := st.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "hook-failover"})
	if err != nil {
		t.Fatal(err)
	}
	f.stopWriter()
	if _, err = primary.opts.Store.ObserveAuthority(t.Context(), promoted.Term); err != nil {
		t.Fatal(err)
	}
	before := len(p.about("record.changed", string(pending.UID)))
	mu.Lock()
	refuse[string(pending.UID)] = false
	mu.Unlock()
	integrationWriter(t, follower)
	var delivered hookPost
	awaitServingRuntime(t, follower, func() bool {
		var row kinds.IntegrationDelivery
		if st.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationDelivery, Name: kinds.DeliveryName(owner, refused[0].id)}, &row) != nil || !row.Spec.Acknowledged {
			return false
		}
		posts := p.about("record.changed", string(pending.UID))
		delivered = posts[len(posts)-1]
		return len(posts) > before
	})
	if delivered.id != refused[0].id || string(delivered.body) != string(refused[0].body) {
		t.Fatal("the successor changed the pending delivery")
	}
	f.signed(t, delivered)
	awaitServingRuntime(t, follower, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, post := range p.posts {
			if post.topic == "cluster.leader_changed" && post.payload["epoch"] == float64(promoted.Term.Epoch) {
				return true
			}
		}
		return false
	})
	time.Sleep(3 * time.Second)
	if n := len(p.about("record.changed", string(acked.UID))); n != 1 {
		t.Fatal("an acknowledged event was delivered again after promotion", n)
	}
}
