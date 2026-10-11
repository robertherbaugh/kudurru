package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// listBody is the adblock list both nodes compile.
const listBody = `! kudurru e2e list
||ads.e2e-blocked.example^
||tracker.e2e-blocked.example^
||metrics.e2e-blocked.example^
`

// TestTwoNodes runs two `kudurru serve` processes with distinct data
// directories and ports, the sqlite driver and one shared
// KUDURRU_ENCRYPTION_KEY, and exercises the internal listener (D47) between
// them with the derived credentials.
//
// The sqlite driver's membership is cluster.Self (serve.go): every sqlite
// node is its own leader and fetches lists itself, so node B cannot be
// configured to pull list bodies from node A today (followup E2E-2). The
// test therefore plays node B's resolver against node A's listener: the
// same Client and Credentials the resolver uses, derived from the shared
// key, fetch A's status and list body by digest.
func TestTwoNodes(t *testing.T) {
	kudurrutest.RequireE2E(t)
	dir := t.TempDir()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	env := []string{"KUDURRU_ENCRYPTION_KEY=" + hex.EncodeToString(key)}

	var listHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		listHits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, listBody)
	}))
	defer srv.Close()
	wantSum := sha256.Sum256([]byte(listBody))
	wantDigest := hex.EncodeToString(wantSum[:])

	objects := []any{
		map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "BlockList",
			"metadata": map[string]any{"name": "e2e-ads", "namespace": "default"},
			"spec": map[string]any{
				"mode":    "block",
				"sources": []any{map[string]any{"url": srv.URL + "/list.txt", "format": "adblock"}},
			},
		},
		map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "Policy",
			"metadata": map[string]any{"name": "e2e-blocking", "namespace": "default"},
			"spec":     map[string]any{"clientGroupRefs": []any{"default"}, "blockListRefs": []any{"e2e-ads"}, "priority": 10},
		},
	}
	var nodes []*kudurrutest.Node
	for _, name := range []string{"node-a", "node-b"} {
		spec, cfg := kudurrutest.NewNodeSpec(t, dir, name)
		cfg["objects"] = objects
		kudurrutest.WriteConfig(t, spec.Config, cfg)
		start := time.Now()
		nodes = append(nodes, kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env}))
		t.Logf("timing: %s ready in %s", name, time.Since(start).Round(time.Millisecond))
	}
	a, b := nodes[0], nodes[1]

	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: key, NodeName: "e2e-client"})
	if err != nil {
		t.Fatal(err)
	}
	client := internalapi.NewClient(creds, internalapi.ClientOptions{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// listDigests waits until node n reports the list compiled.
	listDigests := func(n *kudurrutest.Node) internalapi.Status {
		t.Helper()
		var st internalapi.Status
		if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool {
			st, err = client.Status(ctx, n.Ports.Internal)
			if err != nil {
				return false
			}
			for _, l := range st.Lists {
				if strings.HasSuffix(l.Name, "e2e-ads") && len(l.Digests) > 0 {
					return true
				}
			}
			return false
		}) {
			t.Fatalf("%s: e2e-ads not compiled: status %+v, err %v", n.Name, st, err)
		}
		return st
	}
	stA, stB := listDigests(a), listDigests(b)

	t.Run("Status", func(t *testing.T) {
		if stA.Node != "node-a" || stA.Leader != "node-a" {
			t.Errorf("node A status: node %q leader %q, want node-a/node-a", stA.Node, stA.Leader)
		}
		if stB.Node != "node-b" || stB.Leader != "node-b" {
			t.Errorf("node B status: node %q leader %q (sqlite membership is Self)", stB.Node, stB.Leader)
		}
		if stA.State == "" || stA.Version == "" {
			t.Errorf("status lacks state/version: %+v", stA)
		}
	})

	digestsOf := func(st internalapi.Status) []string {
		for _, l := range st.Lists {
			if strings.HasSuffix(l.Name, "e2e-ads") {
				return l.Digests
			}
		}
		return nil
	}

	t.Run("DigestsMatch", func(t *testing.T) {
		da, db := digestsOf(stA), digestsOf(stB)
		if !slices.Equal(da, db) {
			t.Fatalf("digests differ: node-a %v, node-b %v", da, db)
		}
		if !slices.Contains(da, wantDigest) {
			t.Fatalf("digests %v do not contain sha256(body) %s", da, wantDigest)
		}
	})

	t.Run("PullBody", func(t *testing.T) {
		start := time.Now()
		rc, info, err := client.OpenList(ctx, a.Ports.Internal, wantDigest)
		if err != nil {
			t.Fatalf("GET /internal/lists/%s from node A: %v", wantDigest, err)
		}
		body, err := io.ReadAll(rc) // the reader verifies sha256 against the digest
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read verified body: %v", err)
		}
		if string(body) != listBody {
			t.Fatalf("body differs from the source:\n%q", body)
		}
		if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != wantDigest {
			t.Fatal("hash mismatch")
		}
		if info.Size != int64(len(listBody)) || !strings.Contains(info.Source, srv.URL) {
			t.Errorf("list info %+v", info)
		}
		t.Logf("timing: pulled %d bytes from node A in %s", len(body), time.Since(start).Round(time.Microsecond))
		// Node B holds the same body under the same digest.
		rc, _, err = client.OpenList(ctx, b.Ports.Internal, wantDigest)
		if err != nil {
			t.Fatalf("node B: %v", err)
		}
		_, err = io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("node B body: %v", err)
		}
		// An unknown digest is a 404.
		if _, _, err := client.OpenList(ctx, a.Ports.Internal, strings.Repeat("0", 64)); !errors.Is(err, internalapi.ErrListNotFound) {
			t.Errorf("unknown digest: %v, want ErrListNotFound", err)
		}
	})

	t.Run("Unauthenticated", func(t *testing.T) {
		// TLS verified against the derived CA, but no or a wrong token.
		hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: creds.ClientTLS()}}
		for _, path := range []string{internalapi.PathStatus, internalapi.PathLists + wantDigest} {
			for _, auth := range []string{"", "Bearer not-the-token", "Basic " + creds.Token()} {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+a.Ports.Internal+path, nil)
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				resp, err := hc.Do(req)
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(b), "e2e-blocked") {
					t.Errorf("%s with %q: %d %q, want 401", path, auth, resp.StatusCode, b)
				}
			}
		}
		// Plain HTTP on the TLS listener gets nothing useful.
		resp, err := http.Get("http://" + a.Ports.Internal + internalapi.PathStatus)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Error("plain HTTP status request answered 200")
			}
		}
		// A node with another key cannot even complete the handshake.
		other := make([]byte, 32)
		_, _ = rand.Read(other)
		oc, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: other, NodeName: "intruder"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := internalapi.NewClient(oc, internalapi.ClientOptions{Timeout: 3 * time.Second}).Status(ctx, a.Ports.Internal); err == nil {
			t.Error("a client with a different encryption key read node A's status")
		}
		// Even skipping certificate verification, its token is refused.
		ic := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // deliberately unverified
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+a.Ports.Internal+internalapi.PathStatus, nil)
		req.Header.Set("Authorization", "Bearer "+oc.Token())
		if resp, err := ic.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("foreign token: %d, want 401", resp.StatusCode)
			}
		}
	})

	t.Run("BothNodesBlock", func(t *testing.T) {
		for _, n := range nodes {
			r, err := exchange(n.Ports.DNS, "ads.e2e-blocked.example.", dns.TypeA, false)
			if err != nil {
				t.Fatalf("%s: %v", n.Name, err)
			}
			ip := firstA(r)
			if r.Rcode != dns.RcodeNameError && ip != "0.0.0.0" {
				t.Errorf("%s does not block ads.e2e-blocked.example: %v", n.Name, r)
			}
		}
	})

	t.Run("Isolation", func(t *testing.T) {
		if a.DataDir == b.DataDir {
			t.Fatal("nodes share a data directory")
		}
		for _, n := range nodes {
			c, err := net.DialTimeout("tcp", n.Ports.Internal, time.Second)
			if err != nil {
				t.Errorf("%s internal listener: %v", n.Name, err)
				continue
			}
			_ = c.Close()
		}
		// Each sqlite node fetched the source itself (no list pull between
		// sqlite nodes, E2E-2).
		if n := listHits.Load(); n < 2 {
			t.Errorf("list source fetched %d times, want once per sqlite node", n)
		}
	})
}
