package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// anchorNoImageDigest hashes every file of a closed SQLite data directory, so
// a refused startup is shown to have written nothing at all.
func anchorNoImageDigest(t *testing.T, dir string) [32]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), sqlite.FileName) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(e.Name()))
		h.Write(b)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// anchorNoImageCopy copies a closed data directory (database, member identity,
// invitation) so one fixture can be started under several topologies.
func anchorNoImageCopy(t *testing.T, from, to string) {
	t.Helper()
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

// D370 (F28-EF-2). A node with no installed cluster image never enters the
// managed edge lane (D319), and an unenrolled receiver never enters either
// managed topology. Each gets one specific refusal naming the join commands,
// before any edge observation or root fetch, with its database unchanged. A
// direct follower enrolled without an image (fixture only; serve never creates
// one) keeps its documented wait for installation. After join-reconcile the
// same data directory starts behind the same edges.
func TestAnchorNoImageStartupRefusesWithRemedy(t *testing.T) {
	p, credentials, issue := joinPrimaryFixture(t)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	opts := sqlite.Options{Scheme: scheme, Validator: p.opts.Validator, InternalKinds: privateStoreKinds(), Identity: "backend-a", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage}
	// Each fixture is built once in a template directory owned by the parent
	// test; every subtest starts a copy, so the template incomplete join stays
	// untouched for the reconciliation below.
	var incompleteDir, incompleteFile string
	fixtures := map[string]func(t *testing.T, dir string){
		// A receiver created by the offline join that never sent its proposal.
		"unenrolled": func(t *testing.T, dir string) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			s, err := sqlite.CreateJoinReceiver(ctx, filepath.Join(dir, sqlite.FileName), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if _, err := memberidentity.Create(filepath.Join(dir, memberIdentityFile), s.Authority().MemberID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BindAnchorLifecycle(ctx); err != nil {
				t.Fatal(err)
			}
		},
		// The primary accepted the proposal but the first image never
		// installed (D356): the receiver is still unenrolled with its intent.
		"incomplete-join": func(t *testing.T, dir string) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			s, err := sqlite.CreateJoinReceiver(ctx, filepath.Join(dir, sqlite.FileName), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			local, err := memberidentity.Create(filepath.Join(dir, memberIdentityFile), s.Authority().MemberID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.BindAnchorLifecycle(ctx); err != nil {
				t.Fatal(err)
			}
			inv := issue("backend-a")
			wire, err := inv.Encode()
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "invitation")
			if err := os.WriteFile(file, []byte(wire), 0600); err != nil {
				t.Fatal(err)
			}
			validation := p.schema.PrimaryImageOptions(ctx, inv.ClusterID()).Validation
			refused := errors.New("fixture refuses the first acquired image")
			validation.ValidateImage = func(*replication.Image) error { return refused }
			initial, err := clusterreplica.JoinReceiver(ctx, s, local, inv, credentials, validation)
			if !errors.Is(err, refused) || !initial.Primary.Committed || s.Authority().Mode != replication.Unenrolled {
				t.Fatal("fixture did not reach the accepted-but-uninstalled join", err)
			}
			incompleteDir, incompleteFile = dir, file
		},
		// Store.Enroll without an install: no production path creates it.
		"empty-follower": func(t *testing.T, dir string) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			o := opts
			o.InitialMode = replication.Follower
			s, err := sqlite.Open(filepath.Join(dir, sqlite.FileName), o)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if err := s.Enroll(ctx, p.opts.Store.Authority().Term); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BindAnchorLifecycle(ctx); err != nil {
				t.Fatal(err)
			}
		},
	}
	start := func(t *testing.T, dir, topology string, managed bool) (*servingRuntime, error, *atomic.Int32, *atomic.Int32, *sqlite.Store) {
		t.Helper()
		opening, opened := context.WithTimeout(t.Context(), 10*time.Second)
		st, err := sqlite.OpenExisting(opening, filepath.Join(dir, sqlite.FileName), opts)
		opened()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		var calls atomic.Int32
		observe := func(_ context.Context, id string) (internalapi.EdgeBypassState, error) {
			calls.Add(1)
			return anchorEdgeState(id), nil
		}
		first, client := anchorEdgePeer(t, "edge-a", strings.Repeat("a", 64), observe)
		second, _ := anchorEdgePeer(t, "edge-b", strings.Repeat("b", 64), observe)
		o := edgeAnchorOptions(t, p, []anchorEdgeRequirement{first, second}, client)
		o.Store = st
		o.Config.Node.DataDir = dir
		o.Config.Upstreams.Recursion.ManagedTopology = topology
		if topology == "direct" {
			o.Config.Upstreams.Recursion.ManagedEdges = nil
		}
		seed, reply := anchorWireSignedReply(t)
		endpoint, reads := anchorWireServer(t, "tcp", reply)
		if managed {
			r := &o.Config.Upstreams.Recursion
			r.AnchorMode, r.TrustAnchors, r.RootHints, r.IPv6 = config.RootAnchorsManaged, seed.DS, []string{endpoint.String()}, "off"
			o.Config.EnvLocked = append(o.Config.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		s, err := newServingRuntime(ctx, o)
		if s != nil {
			t.Cleanup(func() { _ = s.Close(context.Background()) })
		}
		return s, err, &calls, reads, st
	}
	parent, templates := t, map[string]string{}
	for _, name := range []string{"unenrolled", "incomplete-join", "empty-follower"} {
		for _, topology := range []string{"edges", "direct"} {
			t.Run(name+"/"+topology, func(t *testing.T) {
				if templates[name] == "" {
					templates[name] = parent.TempDir()
					fixtures[name](t, templates[name])
				}
				dir := t.TempDir()
				anchorNoImageCopy(t, templates[name], dir)
				before := anchorNoImageDigest(t, dir)
				s, err, calls, reads, st := start(t, dir, topology, true)
				authority := st.Authority()
				if name == "empty-follower" && topology == "direct" {
					// The existing direct wait (TestDirectAnchorEnrolledFollowerWaitsWithoutInitializing).
					if err != nil || s == nil {
						t.Fatal("direct empty follower lost its wait for installation", err)
					}
					if calls.Load() != 0 || reads.Load() != 0 {
						t.Fatal("waiting direct follower contacted an edge or fetched the root", calls.Load(), reads.Load())
					}
					return
				}
				if !errors.Is(err, errAnchorNoImage) {
					t.Fatal("image-less start did not get the specific refusal", err)
				}
				for _, remedy := range []string{"no installed cluster image", "kudurru cluster join --offline", "kudurru cluster join-reconcile --offline"} {
					if !strings.Contains(err.Error(), remedy) {
						t.Fatalf("refusal %q does not name %q", err, remedy)
					}
				}
				if calls.Load() != 0 || reads.Load() != 0 {
					t.Fatal("refused image-less start contacted an edge or fetched the root", calls.Load(), reads.Load())
				}
				if !reflect.DeepEqual(authority, st.Authority()) || authority.Installed != (replication.Generation{}) {
					t.Fatal("refused start changed authority", st.Authority())
				}
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				if anchorNoImageDigest(t, dir) != before {
					t.Fatal("refused image-less start wrote to the database")
				}
			})
		}
	}
	// The remedy works on the same data directory: join-reconcile installs
	// the first image, after which the node starts behind its complete fleet.
	if incompleteDir == "" {
		t.Fatal("incomplete-join fixture did not run")
	}
	var output bytes.Buffer
	cmd := newClusterCmd(&cliOpts{})
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"join-reconcile", "--offline", "--data-dir", incompleteDir, "--invitation-file", incompleteFile, "--confirm-name", "backend-a", "--confirm-address", "127.0.0.2:8082"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal("join-reconcile refused", err)
	}
	var result clusterreplica.JoinReconciliationResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Observed || result.Installed.Validate() != nil {
		t.Fatal("join-reconcile did not install the first image", err)
	}
	s, err, calls, reads, st := start(t, incompleteDir, "edges", false)
	if err != nil {
		t.Fatal("reconciled receiver did not start behind its edges", err)
	}
	if st.Authority().Mode != replication.Follower || calls.Load() != 2 || reads.Load() != 0 || s.anchors.admission == nil || s.anchors.admission.edges == nil || !s.anchors.admission.current() {
		t.Fatal("reconciled follower did not adopt one complete fleet observation", st.Authority().Mode, calls.Load())
	}
}
