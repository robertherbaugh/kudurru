package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// F28-EF-1 (D370). The supported order for a new follower behind managed
// edges: the primary has bootstrapped managed trust, the receiver is joined
// with the real offline `cluster join` CLI (its first image carries the
// managed owner), and only then is the follower started with edges topology.
// Startup authenticates the whole fleet once, each edge listing this backend
// exactly once, adopts that observation and mounts the deny-only supervisor,
// which makes one root acquisition and publishes the accepted managed root.
// A physical restart re-authenticates the complete fleet and serves the same
// accepted owner.
func TestAnchorEdgeActivationJoinedFollower(t *testing.T) {
	endpoint, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
	primary, _ := anchorBootstrappedPrimary(t, endpoint, seeds)
	if reads.Load() != 1 {
		t.Fatal("primary did not bootstrap with one acquisition", reads.Load())
	}
	_, issue := joinPrimaryOn(t, primary, time.Minute)
	inv := issue("backend-a")
	wire, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "invitation")
	if err := os.WriteFile(file, []byte(wire), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backend-a")
	var output bytes.Buffer
	cmd := newClusterCmd(&cliOpts{})
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"join", "--offline", "--data-dir", dir, "--invitation-file", file, "--confirm-name", "backend-a", "--confirm-address", "127.0.0.2:8082"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal("offline join failed", err)
	}
	var result clusterreplica.JoinResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Primary.Committed || result.Installed.Validate() != nil {
		t.Fatal("offline join did not commit and install", err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	open := func() *sqlite.Store {
		t.Helper()
		opening, opened := context.WithTimeout(t.Context(), 10*time.Second)
		defer opened()
		st, err := sqlite.OpenExisting(opening, filepath.Join(dir, sqlite.FileName), sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	st := open()
	if st.Authority().Mode != replication.Follower || !st.Authority().Installed.Same(result.Installed) {
		t.Fatal("joined receiver is not an installed follower", st.Authority().Mode)
	}
	image, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil || owner.Inspect().Phase != accepted.Managed {
		t.Fatal("first image does not carry the primary's managed owner", err)
	}

	var calls atomic.Int32
	observe := func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
		calls.Add(1)
		return anchorEdgeState(id), nil
	}
	first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), observe)
	second, _ := anchorEdgePeer(t, "edge-b", strings.Repeat("b", 64), observe)
	opts := edgeAnchorOptions(t, primary, []anchorEdgeRequirement{first, second}, client)
	opts.Config.Node.DataDir = dir
	// The follower has its own transferred-zone runtime, not the primary's.
	opts.Transferred = servingbuild.NewTransferredRuntime(nil, nil, nil)
	r := &opts.Config.Upstreams.Recursion
	r.AnchorMode, r.TrustAnchors, r.RootHints, r.IPv6 = config.RootAnchorsManaged, seeds, []string{endpoint}, "off"
	opts.Config.EnvLocked = append(opts.Config.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")

	// start runs one physical start of the follower and returns its stop.
	start := func(st *sqlite.Store, wantCalls, wantReads int32) func() {
		t.Helper()
		o := opts
		o.Store = st
		startup, finish := context.WithTimeout(t.Context(), 10*time.Second)
		s, err := newServingRuntime(startup, o)
		finish()
		if err != nil {
			t.Fatal("joined follower did not start behind its edges", err)
		}
		if calls.Load() != wantCalls || s.anchors.admission == nil || s.anchors.admission.edges == nil || !s.anchors.admission.current() {
			t.Fatal("joined follower did not adopt one complete fleet observation", calls.Load())
		}
		run, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Run(run) }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error("follower runtime completion", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("follower runtime did not join")
			}
			closing, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			if err := s.Close(closing); err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(stop)
		// The deny-only supervisor made its own acquisition and the runtime
		// publishes the accepted managed root through the output guard.
		awaitServingRuntime(t, s, func() bool { return reads.Load() >= wantReads && s.publisher.Status().ProtectedReady })
		lease, err := s.publisher.Capture()
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		part, _ := lease.Bundle().Component(recursiveRootComponent)
		seal, _ := part.(*recursiveRootSeal)
		if seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || lease.Bundle().DNSOnly() {
			t.Fatal("joined follower behind edges does not serve the accepted managed root")
		}
		query, qdone := context.WithTimeout(t.Context(), 10*time.Second)
		defer qdone()
		out, err := captureRootAnchorOutput(&pipeline.Request{Ctx: query, Serving: servingdns.Wrap(lease)})
		if err != nil {
			t.Fatal("output capture", err)
		}
		if err := out.Emit(func() error { return nil }); err != nil {
			t.Fatal("joined follower refused its managed output", err)
		}
		if got := anchorOperationOwner(t.Context(), t, s).Version(); got != owner.Version() {
			t.Fatal("follower serves a different owner than its installed image", got)
		}
		return stop
	}
	stop := start(st, 2, 2)
	// Exactly one supervisor acquisition and no further fleet contact inside
	// the supervisor's cooldown.
	time.Sleep(1200 * time.Millisecond)
	if reads.Load() != 2 || calls.Load() != 2 {
		t.Fatal("joined follower reacquired the root or replayed its fleet", reads.Load(), calls.Load())
	}
	stop()
	before := st.Authority()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Physical restart: a new process start observes the whole fleet again.
	st = open()
	if st.Authority().Mode != replication.Follower || !st.Authority().Installed.Same(before.Installed) {
		t.Fatal("restart changed follower authority")
	}
	stop = start(st, 4, 2)
	stop()
	// The restarted supervisor may make at most one new acquisition.
	if calls.Load() != 4 || reads.Load() > 3 {
		t.Fatal("restart did not observe the complete fleet exactly once more, or reacquired", calls.Load(), reads.Load())
	}
}
