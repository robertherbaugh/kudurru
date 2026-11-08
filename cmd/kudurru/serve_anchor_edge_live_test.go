package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This is a separately enabled actual daemon composition, not a mocked edge
// observation. Only its own pinned-image child and scratch directory are owned.
func TestRunServeManagedEdgePinnedDaemonRestart(t *testing.T) {
	if os.Getenv("KUDURRU_ANCHOR_EDGE_LIVE") != "1" {
		t.Skip("set KUDURRU_ANCHOR_EDGE_LIVE=1 for pinned dnsdist/backend activation acceptance")
	}
	parent := os.Getenv("KUDURRU_ANCHOR_EDGE_WORKDIR")
	if !filepath.IsAbs(parent) {
		t.Fatal("absolute owned edge scratch parent required")
	}
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled, cfg.DNSSEC.Enabled = false, false
	cfg.Cache.MaxEntries = 0
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	credentials, err := edge.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(secret, []byte(credentials.Password), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Listeners.ProxyProtocolSecretFile = secret
	seed, reply := anchorWireSignedReply(t)
	root, reads := anchorWireServer(t, "tcp", reply)
	r := &cfg.Upstreams.Recursion
	r.ManagedTopology, r.AnchorMode, r.DNSSEC = "edges", config.RootAnchorsManaged, true
	r.TrustAnchors, r.RootHints, r.IPv6 = seed.DS, []string{root.String()}, "off"
	backend := cluster.Endpoint{Name: cfg.Node.Name, Address: cfg.Listeners.DNS[0].Address, InternalAddress: cfg.Listeners.Internal.Address, Ready: true, Serving: true}
	listen := offlineFreeAddress(t, "tcp")
	_, portText, _ := net.SplitHostPort(listen)
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(parent, "root-edge-")
	if err != nil {
		t.Fatal(err)
	}
	name := "kudurru-root-edge-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const image = "sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e"
	fixture, err := json.Marshal(map[string]string{"name": name, "image": image})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), fixture, 0o600); err != nil { //nolint:gosec // generated private owned fixture
		t.Fatal(err)
	}
	wrapper, err := os.ReadFile("../../ee/edge/dnsdist/testdata/console-client.py")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "dnsdist-client")
	if err := os.WriteFile(binary, wrapper, 0o700); err != nil { //nolint:gosec // fixed tracked wrapper must execute in owned scratch
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "--host", "unix:///tmp/kudurru-lab-docker.sock", "rm", "--force", name).Run() //nolint:gosec // only this generated owned fixture on fixed test daemon
	})
	edgeConfig := dnsdist.Config{Options: edge.Options{ManagedCacheBypass: true, AuthoritativeCache: true, AuthoritativeZones: []string{"anchor.test."}, Listen: []string{"127.0.0.1"}, Protocols: []edge.Protocol{edge.Do53}, Ports: map[edge.Protocol]int{edge.Do53: port}, API: offlineFreeAddress(t, "tcp"), Console: offlineFreeAddress(t, "tcp"), ProxyProtocolSecret: credentials.Password}, Source: &cluster.StaticSource{Endpoints: []cluster.Endpoint{backend}}, ConfigDir: filepath.Join(dir, "config"), Binary: binary, ChildEnv: []string{"KUDURRU_F43_LAB=" + dir}, StatusPoll: 100 * time.Millisecond, Debounce: 100 * time.Millisecond, StopTimeout: 10 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	startEdge := func() (*dnsdist.Supervisor, func(), internalapi.EdgeBypassState) {
		t.Helper()
		sup, err := dnsdist.New(edgeConfig)
		if err != nil {
			t.Fatal(err)
		}
		life, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- sup.Run(life) }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("owned edge completion", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("owned edge did not join")
			}
		}
		t.Cleanup(stop)
		until := time.Now().Add(15 * time.Second)
		for time.Now().Before(until) {
			rec := httptest.NewRecorder()
			sup.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
			var status struct {
				ID string `json:"managedCacheBypassIdentity"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &status) == nil && status.ID != "" {
				if observed, err := sup.ManagedBypass(t.Context(), status.ID); err == nil {
					return sup, stop, observed
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("actual edge failed installed acknowledgment before backend startup")
		return nil, nil, internalapi.EdgeBypassState{}
	}
	sup, stopEdge, installed := startEdge()
	// The test obtains the operator's expected retained identity before starting
	// the backend. Production still makes its own original authenticated request.
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{8}, 32), NodeName: "edge-live"})
	if err != nil {
		t.Fatal(err)
	}
	observationAddress := offlineFreeAddress(t, "tcp")
	startObservation := func(sup *dnsdist.Supervisor) func() {
		t.Helper()
		server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, EdgeBypass: sup.ManagedBypass})
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", observationAddress)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- server.Serve(ctx, ln) }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(stop)
		return stop
	}
	stopObservation := startObservation(sup)
	r.ManagedEdges = []config.ManagedRootEdge{{Node: "edge-live", Address: observationAddress, Deployment: installed.Deployment, BackendAddress: backend.Address, BackendInternalAddress: backend.InternalAddress}}
	st, closeStore := tsigMountedStore(t, cfg)
	for i, zone := range []string{"anchor.test.", "resolver.test."} {
		zoneName := "edge-zone-" + strconv.Itoa(i)
		for _, obj := range []store.Object{
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: zoneName}, Spec: public.DNSZoneSpec{ZoneName: zone, Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + zone}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
			&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: zoneName}, Endpoints: []public.Endpoint{{DNSName: "answer." + zone, RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.77"}}}}},
		} {
			if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
		}
	}
	closeStore()
	stopBackend := tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")
	ask := func(live bool) {
		t.Helper()
		for _, protocol := range []string{"udp", "tcp"} {
			for _, zone := range []string{"anchor.test.", "resolver.test."} {
				q := new(dns.Msg)
				q.SetQuestion("answer."+zone, dns.TypeA)
				answer, _, err := (&dns.Client{Net: protocol, Timeout: time.Second}).Exchange(q, listen)
				if live {
					if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
						t.Fatal("actual edge/backend answer missing", protocol, zone, err)
					}
					if a, ok := answer.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.77" {
						t.Fatal("actual edge/backend answer changed")
					}
				} else if err == nil && answer != nil && len(answer.Answer) != 0 {
					t.Fatal("edge retained a cached answer after backend joined", protocol, zone)
				}
			}
		}
	}
	waitEdge := func(s *dnsdist.Supervisor) {
		t.Helper()
		// Health recovery is separate from the installed bypass acknowledgment.
		until := time.Now().Add(10 * time.Second)
		for ready, _ := s.Ready(); !ready; ready, _ = s.Ready() {
			if time.Now().After(until) {
				t.Fatal("edge backend health did not converge")
			}
			time.Sleep(20 * time.Millisecond)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
		var status struct {
			DNSDist dnsdist.ServerStatus `json:"dnsdist"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		pools := 0
		for _, pool := range status.DNSDist.Pools {
			if pool.Name == edge.PoolResolver || pool.Name == edge.PoolAuthoritative {
				pools++
				if pool.CacheSize != 0 || pool.CacheEntries != 0 {
					t.Fatal("actual managed edge retained a cache", pool.Name)
				}
			}
		}
		if pools != 2 {
			t.Fatal("actual edge did not expose both pools")
		}
	}
	waitEdge(sup)
	ask(true)
	ask(true)
	stopObservation()
	stopEdge()
	next, _, after := startEdge()
	if after.Deployment != installed.Deployment || after.Run == installed.Run {
		t.Fatal("edge restart lost retained deployment or fresh run identity")
	}
	startObservation(next)
	// Child restart is protected by the deployment's immutable cache policy;
	// it cannot renew the already completed backend startup observation.
	waitEdge(next)
	ask(true)
	stopBackend()
	checkpoint := func() accepted.Version {
		t.Helper()
		st, closeStore := tsigMountedStore(t, cfg)
		defer closeStore()
		image, err := st.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil || owner.Inspect().Phase != accepted.Managed {
			t.Fatal("actual edge-backed store lost managed owner", err)
		}
		return owner.Version()
	}
	version := checkpoint()
	ask(false)
	stopBackend = tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")
	waitEdge(next)
	ask(true)
	stopBackend()
	if reads.Load() != 1 || checkpoint() != version {
		t.Fatal("edge or backend restart bootstrapped again or advanced accepted anchors", reads.Load())
	}
}
