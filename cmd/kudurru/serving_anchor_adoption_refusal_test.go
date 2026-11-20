package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type anchorRefusalSource struct{ image *replication.Image }

func (s *anchorRefusalSource) Fetch(context.Context, string) (follower.Response, error) {
	return follower.Response{Term: s.image.Term, Image: s.image, ETag: s.image.Generation().ETag()}, nil
}

func anchorRefusalBearer(ctx context.Context, t *testing.T, s *servingRuntime) string {
	t.Helper()
	token, id := tokens.Generate()
	created := metav1.NewTime(time.Now().Add(-time.Minute))
	expires := metav1.NewTime(time.Now().Add(time.Hour))
	err := s.opts.Store.Command(ctx, s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			return err
		}
		user, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "anchor-reviewer", CreationTimestamp: created}, Spec: internalv1.LocalUserSpec{BuiltinAdmin: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: created}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(token), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:" + user.GetName(), Provider: "local"}, Scopes: []string{"admin"}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func mountAnchorRefusalFollower(t *testing.T, f *anchorFollowerFixture, source *anchorRefusalSource, bearer string) (func() *httptest.ResponseRecorder, func() int) {
	t.Helper()
	var err error
	f.s.member, err = clusterreplica.NewCoordinator(clusterreplica.CoordinatorOptions{
		Store: f.store, Source: source, NodeName: f.store.Authority().MemberID, Served: f.s.publisher.Pin,
		WithOperator: f.s.withPromotionAuthorization, ObservePull: f.s.anchorStatus.observePull,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s.memberPullStart = make(chan struct{})
	runServingFixture(t, f.s)
	h, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1)})
	if err != nil {
		t.Fatal(err)
	}
	router := h.Middleware(h.Handler())
	healthz := health.New(nil)
	healthz.Register("serving", f.s.ready)
	return func() *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, api.BasePath+"/cluster/anchors", nil)
			req.Header.Set("Authorization", "Bearer "+bearer)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			return res
		}, func() int {
			res := httptest.NewRecorder()
			healthz.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/healthz/ready", nil))
			return res.Code
		}
}

func anchorRefusalImage(t *testing.T, image *replication.Image, owner accepted.Snapshot) *replication.Image {
	t.Helper()
	rows, err := accepted.EncodeRows(owner)
	if err != nil {
		t.Fatal(err)
	}
	replacement := *image
	if updated := owner.Inspect().UpdatedAt; replacement.CreatedAt.Before(updated) {
		replacement.CreatedAt = updated
	}
	replacement.Payload.Runtime = rows
	for _, row := range image.Payload.Runtime {
		if row.Key != accepted.RootKey && !strings.HasPrefix(row.Key, accepted.ChunkPrefix) {
			replacement.Payload.Runtime = append(replacement.Payload.Runtime, row)
		}
	}
	if err := replication.Seal(&replacement); err != nil {
		t.Fatal(err)
	}
	return &replacement
}

func TestAnchorRefusalsThroughMountedStatus(t *testing.T) {
	root := newAnchorHistoryAuthority(t)
	primary, original, target, host := anchorOperationFixtureTopology(t, root.endpoint, root.seeds, "direct")
	bearer := anchorRefusalBearer(original, t, primary)
	writer := newAnchorHistoryWriter(original, t, primary, target, host)
	writer.operate(t, root, false)
	bootstrap := anchorOperationOwner(original, t, primary)
	writer.operate(t, root, false)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	t.Cleanup(cancel)
	f := newAnchorFollowerFixture(ctx, t, primary)
	before := anchorFollowerImage(ctx, t, f.store)
	authority := f.store.Authority()
	writer.operate(t, root, false)
	canonical := anchorFollowerImage(ctx, t, primary.opts.Store)
	source := &anchorRefusalSource{image: canonical}
	status, ready := mountAnchorRefusalFollower(t, f, source, bearer)
	anchorHistoryAwait(t, "follower did not publish and check the root once", f.s, root, func() bool {
		return f.s.publisher.Status().ProtectedReady && root.reads.Load() == 4
	})
	if res := status(); res.Code != http.StatusOK || ready() != http.StatusOK {
		t.Fatalf("complete follower should admit protected reads: %d %s", res.Code, res.Body)
	}

	shared := bootstrap.Inspect()
	dormant, err := accepted.NewDormant(shared.ClusterID, shared.HistoryID, shared.InitializedAt)
	if err != nil {
		t.Fatal(err)
	}
	otherHistory, err := accepted.NewDormant(shared.ClusterID, strings.Repeat("d", 32), shared.InitializedAt)
	if err != nil {
		t.Fatal(err)
	}
	state, err := bootstrap.State()
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := accepted.ProposeManaged(otherHistory, state, shared.UpdatedAt, shared.ObservationUntil)
	if err != nil {
		t.Fatal(err)
	}
	seed, signed := anchorWireSignedReply(t)
	seed.ClusterID = shared.ClusterID
	request := new(dns.Msg).SetQuestion(".", dns.TypeDNSKEY).SetEdns0(1232, true)
	request.RecursionDesired = false
	wire, err := request.Pack()
	if err != nil {
		t.Fatal(err)
	}
	reply := new(dns.Msg)
	if err := reply.Unpack(signed(wire)); err != nil {
		t.Fatal(err)
	}
	sample := writer.clock.Sample()
	state, proof, err := trustanchors.Bootstrap(seed, reply, sample)
	if err != nil {
		t.Fatal(err)
	}
	otherPolicy, err := accepted.ProposeManaged(dormant, state, state.Inspect().Checkpoint.Wall, proof.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	reg := anchorStatusMetrics(t, f.s)
	for _, test := range []struct {
		reason sqlite.AnchorRefusalReason
		owner  accepted.Snapshot
	}{
		{sqlite.AnchorRefusedHistory, foreign},
		{sqlite.AnchorRefusedPolicy, otherPolicy},
		{sqlite.AnchorRefusedDormantOwner, dormant},
		{sqlite.AnchorRefusedNotSuccessor, bootstrap},
	} {
		t.Run(string(test.reason), func(t *testing.T) {
			source.image = anchorRefusalImage(t, canonical, test.owner)
			err := f.s.member.Pull(ctx)
			assertAnchorRefusal(t, err, test.reason)
			if !reflect.DeepEqual(authority, f.store.Authority()) || !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) {
				t.Fatal("refused pull changed installed state or authority")
			}
			res := status()
			var got api.RootAnchorStatus
			if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &got) != nil || got.Pull == nil || got.Pull.Result != "refused" || got.Pull.Refusal != string(test.reason) || !got.AdmissionCurrent || ready() != http.StatusOK {
				t.Fatalf("protected status did not report refusal over the retained generation: %d %s", res.Code, res.Body)
			}
			for _, reason := range anchorRefusalReasons {
				want := 0.0
				if reason == test.reason {
					want = 1
				}
				if got := anchorStatusGauge(t, reg, "kudurru_root_anchor_adoption_refused", map[string]string{"reason": string(reason)}); got != want {
					t.Fatalf("refusal metric for %s = %v, want %v", reason, got, want)
				}
			}
		})
	}
	if got := root.reads.Load(); got != 4 {
		t.Fatalf("three writer acquisitions and one supervision acquisition expected, got %d", got)
	}
}

func assertAnchorRefusal(t *testing.T, err error, want sqlite.AnchorRefusalReason) *sqlite.AnchorRefusal {
	t.Helper()
	var refusal *sqlite.AnchorRefusal
	if !errors.As(err, &refusal) || refusal.Reason != want || !errors.Is(err, accepted.ErrCAS) {
		t.Fatalf("pull refusal = %v, want %s", err, want)
	}
	return refusal
}

func TestAnchorRefusalRetiredFollowerKeepsProtectedStatusClosed(t *testing.T) {
	root := newAnchorHistoryAuthority(t)
	primary, original, target, host := anchorOperationFixtureTopology(t, root.endpoint, root.seeds, "direct")
	bearer := anchorRefusalBearer(original, t, primary)
	writer := newAnchorHistoryWriter(original, t, primary, target, host)
	writer.operate(t, root, false)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	t.Cleanup(cancel)
	f := newAnchorFollowerFixture(ctx, t, primary)
	before := anchorFollowerImage(ctx, t, f.store)
	writer.operate(t, root, false)
	source := &anchorRefusalSource{image: anchorFollowerImage(ctx, t, primary.opts.Store)}
	root.revoke.Store(true)
	status, ready := mountAnchorRefusalFollower(t, f, source, bearer)
	anchorHistoryAwait(t, "follower did not retire on the signed REVOKE", f.s, root, func() bool {
		return f.s.anchorStatus.snapshot().supervisor.state == anchorWorkerRetired
	})
	err := f.s.member.Pull(ctx)
	refusal := assertAnchorRefusal(t, err, sqlite.AnchorRefusedCarriedRevocation)
	if len(refusal.Keys) != 1 || !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("pull did not refuse exactly one key without changing the installed image")
	}
	if res := status(); res.Code != http.StatusServiceUnavailable || ready() != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), string(refusal.Reason)) {
		t.Fatalf("retired follower must close protected status and readiness without exposing the refusal: %d %s", res.Code, res.Body)
	}
	reg := anchorStatusMetrics(t, f.s)
	if got := anchorStatusGauge(t, reg, "kudurru_root_anchor_adoption_refused", map[string]string{"reason": string(refusal.Reason)}); got != 1 {
		t.Fatal("retired follower refusal metric", got)
	}
	if got := root.reads.Load(); got != 3 {
		t.Fatalf("two writer acquisitions and one denial acquisition expected, got %d", got)
	}
}

func TestAnchorRefusalMissingRecoveryKeepsProtectedStatus(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 2, nil)
	primary, original, target, host := anchorOperationFixtureTopology(t, endpoint, seeds, "direct")
	bearer := anchorRefusalBearer(original, t, primary)
	writer := newAnchorHistoryWriter(original, t, primary, target, host)
	for _, revoked := range []bool{false, true} {
		op, err := primary.captureAnchorOperation(original, target, host, writer.clock, 0)
		if err != nil || op == nil {
			t.Fatal("original root operation", err)
		}
		result, err := op.run()
		if err != nil || result.Update.Outcome() != sqlite.AnchorCommitted || (result.Denial.Outcome() == sqlite.AnchorCommitted) != revoked {
			t.Fatal("root operation did not commit the expected branch", err, result)
		}
	}
	lost := anchorOperationOwner(original, t, primary)
	state, err := lost.State()
	if err != nil || !state.Inspect().Quarantined {
		t.Fatal("fixture did not lose its original trust", err)
	}
	replacement := newAnchorHistoryAuthority(t)
	update, err := primary.opts.Store.CaptureAnchorUpdate(original, lost.Version())
	if err != nil {
		t.Fatal(err)
	}
	transition, err := persistence.CaptureTransition(original, lost.Version(), writer.clock)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := captureAnchorWirePlan(original, []netip.AddrPort{netip.MustParseAddrPort(replacement.endpoint)}, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, received, err := acquireAnchorWire(wire, func() (trustanchors.Sample, error) { return writer.clock.Sample(), nil })
	if err != nil {
		t.Fatal(err)
	}
	reply, err := unpackAnchorBootstrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	recovered, proof, err := trustanchors.Recover(state, replacement.seeds, reply, received)
	if err != nil {
		t.Fatal(err)
	}
	result, err := primary.opts.Store.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Recover(ctx, tx, transition, recovered, proof)
		return err
	})
	if err != nil || result.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("fixture recovery did not commit", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	t.Cleanup(cancel)
	f := newAnchorFollowerFixture(ctx, t, primary)
	cfg, err := config.FromFlat(config.Flatten(f.runtime.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.EnvLocked = f.runtime.Config.EnvLocked
	cfg.Upstreams.Recursion.RootHints = []string{replacement.endpoint}
	f.runtime.Config = cfg
	f.reopen(ctx, t)
	before := anchorFollowerImage(ctx, t, f.store)
	newer := *before
	newer.Revision++
	source := &anchorRefusalSource{image: anchorRefusalImage(t, &newer, lost)}
	status, ready := mountAnchorRefusalFollower(t, f, source, bearer)
	anchorHistoryAwait(t, "recovered follower did not publish and check the replacement root once", f.s, replacement, func() bool {
		return f.s.publisher.Status().ProtectedReady && replacement.reads.Load() == 2
	})
	err = f.s.member.Pull(ctx)
	assertAnchorRefusal(t, err, sqlite.AnchorRefusedRecoveryHistory)
	res := status()
	var got api.RootAnchorStatus
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &got) != nil || got.Pull == nil || got.Pull.Result != "refused" || got.Pull.Refusal != string(sqlite.AnchorRefusedRecoveryHistory) || ready() != http.StatusOK {
		t.Fatalf("recovered follower did not report the refusal over its protected lease: %d %s", res.Code, res.Body)
	}
	reg := anchorStatusMetrics(t, f.s)
	if got := anchorStatusGauge(t, reg, "kudurru_root_anchor_adoption_refused", map[string]string{"reason": string(sqlite.AnchorRefusedRecoveryHistory)}); got != 1 {
		t.Fatal("recovery history refusal metric", got)
	}
	if !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) || reads.Load() != 2 || replacement.reads.Load() != 2 {
		t.Fatal("refused recovery history changed installed image or exact acquisition counts", reads.Load(), replacement.reads.Load())
	}
}
