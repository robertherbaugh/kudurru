package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// anchorEdgeLab owns one pinned dnsdist edge child (through the tracked
// console wrapper on the fixed test daemon), its scratch directory and the
// authenticated observation listener backends use for managed admission.
// Nothing outside the generated container name and directory is touched.
type anchorEdgeLab struct {
	t           *testing.T
	listen      string // Do53 frontend, UDP and TCP
	observation string // internal observation listener address
	config      dnsdist.Config
	creds       *internalapi.Credentials
	// observations counts authenticated managed-bypass observations served
	// to backends; each backend start observes each edge exactly once.
	observations atomic.Int32
	// edge is the supervisor the observation listener serves; nil while the
	// edge child restarts, when an observation is counted and refused.
	edge atomic.Pointer[dnsdist.Supervisor]
}

// newAnchorEdgeLab prepares the edge and configures cfg as a backend behind
// it: PROXYv2 from loopback with the edge's shared secret and a fixed
// internal listener. It returns the secret file other backends must reuse.
func newAnchorEdgeLab(t *testing.T, cfg *config.Config) (*anchorEdgeLab, string) {
	t.Helper()
	if os.Getenv("KUDURRU_ANCHOR_EDGE_LIVE") != "1" {
		t.Skip("set KUDURRU_ANCHOR_EDGE_LIVE=1 for pinned dnsdist/backend activation acceptance")
	}
	parent := os.Getenv("KUDURRU_ANCHOR_EDGE_WORKDIR")
	if !filepath.IsAbs(parent) {
		t.Fatal("absolute owned edge scratch parent required")
	}
	credentials, err := edge.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(secret, []byte(credentials.Password), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	cfg.Listeners.ProxyProtocolSecretFile = secret
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
	// A shared lab daemon may ask for its own prefix so the run's container
	// is attributable; the generated suffix keeps every name unique.
	prefix := "kudurru-root-edge"
	if p := os.Getenv("KUDURRU_ANCHOR_EDGE_NAME_PREFIX"); p != "" {
		prefix = p
	}
	name := prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const image = kudurrutest.DnsdistImage
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
	// The test obtains the operator's expected retained identity before starting
	// a backend. Production still makes its own original authenticated request.
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{8}, 32), NodeName: "edge-live"})
	if err != nil {
		t.Fatal(err)
	}
	lab := &anchorEdgeLab{t: t, listen: listen, observation: offlineFreeAddress(t, "tcp"), creds: creds,
		config: dnsdist.Config{Options: edge.Options{ManagedCacheBypass: true, AuthoritativeCache: true, AuthoritativeZones: []string{"anchor.test."}, Listen: []string{"127.0.0.1"}, Protocols: []edge.Protocol{edge.Do53}, Ports: map[edge.Protocol]int{edge.Do53: port}, API: offlineFreeAddress(t, "tcp"), Console: offlineFreeAddress(t, "tcp"), ProxyProtocolSecret: credentials.Password}, ConfigDir: filepath.Join(dir, "config"), Binary: binary, ChildEnv: []string{"KUDURRU_F43_LAB=" + dir}, StatusPoll: 100 * time.Millisecond, Debounce: 100 * time.Millisecond, StopTimeout: 10 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	return lab, secret
}

// backend is the static --backends entry of one kudurru node.
func (l *anchorEdgeLab) backend(cfg *config.Config) cluster.Endpoint {
	return cluster.Endpoint{Name: cfg.Node.Name, Address: cfg.Listeners.DNS[0].Address, InternalAddress: cfg.Listeners.Internal.Address, Ready: true, Serving: true}
}

// managedEdge is the host-only managedEdges declaration of cfg's backend.
func (l *anchorEdgeLab) managedEdge(cfg *config.Config, deployment string) config.ManagedRootEdge {
	return config.ManagedRootEdge{Node: "edge-live", Address: l.observation, Deployment: deployment, BackendAddress: cfg.Listeners.DNS[0].Address, BackendInternalAddress: cfg.Listeners.Internal.Address}
}

// start runs one edge child with exactly these static backends (the
// operator's --backends list) and waits for its installed bypass
// acknowledgment. The retained bypass lives in the shared config directory.
func (l *anchorEdgeLab) start(backends ...cluster.Endpoint) (*dnsdist.Supervisor, func(), internalapi.EdgeBypassState) {
	t := l.t
	t.Helper()
	c := l.config
	c.Source = &cluster.StaticSource{Endpoints: backends}
	sup, err := dnsdist.New(c)
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

// observe serves sup's installed managed bypass on the fixed observation
// address under the edge's internal credentials. lab.edge switches the
// served supervisor without closing the listener.
func (l *anchorEdgeLab) observe(sup *dnsdist.Supervisor) func() {
	t := l.t
	t.Helper()
	l.edge.Store(sup)
	server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: l.creds, EdgeBypass: func(ctx context.Context, deployment string) (internalapi.EdgeBypassState, error) {
		l.observations.Add(1)
		current := l.edge.Load()
		if current == nil {
			return internalapi.EdgeBypassState{}, errors.New("edge child restarting")
		}
		return current.ManagedBypass(ctx, deployment)
	}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", l.observation)
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

// status reads the supervisor's own dnsdist API status.
func (l *anchorEdgeLab) status(s *dnsdist.Supervisor) dnsdist.ServerStatus {
	t := l.t
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var status struct {
		DNSDist dnsdist.ServerStatus `json:"dnsdist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status.DNSDist
}

// wait waits for edge health and checks that both managed pools are
// uncached.
func (l *anchorEdgeLab) wait(s *dnsdist.Supervisor) {
	t := l.t
	t.Helper()
	// Health recovery is separate from the installed bypass acknowledgment.
	until := time.Now().Add(10 * time.Second)
	for ready, _ := s.Ready(); !ready; ready, _ = s.Ready() {
		if time.Now().After(until) {
			t.Fatal("edge backend health did not converge")
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.uncached(l.status(s))
}

func (l *anchorEdgeLab) uncached(status dnsdist.ServerStatus) {
	t := l.t
	t.Helper()
	pools := 0
	for _, pool := range status.Pools {
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

// states waits until dnsdist reports each listed backend address in the
// wanted health state (true is up), then checks both pools are uncached.
func (l *anchorEdgeLab) states(s *dnsdist.Supervisor, want map[string]bool) {
	t := l.t
	t.Helper()
	until := time.Now().Add(30 * time.Second)
	for {
		status := l.status(s)
		matched := 0
		for _, server := range status.Servers {
			if up, ok := want[server.Address]; ok && server.Routed() && server.Up() == up {
				matched++
			}
		}
		if matched == len(want) {
			l.uncached(status)
			return
		}
		if time.Now().After(until) {
			t.Fatal("edge backend states did not converge", want, status.Servers)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ask queries both pools (anchor.test. is authoritative-routed,
// resolver.test. goes to the resolver pool) over UDP and TCP through the
// edge. live requires the backend answer; otherwise no answer may come back,
// which proves the edge holds no cached copy.
func (l *anchorEdgeLab) ask(live bool) {
	t := l.t
	t.Helper()
	for _, protocol := range []string{"udp", "tcp"} {
		for _, zone := range []string{"anchor.test.", "resolver.test."} {
			q := new(dns.Msg)
			q.SetQuestion("answer."+zone, dns.TypeA)
			answer, _, err := (&dns.Client{Net: protocol, Timeout: time.Second}).Exchange(q, l.listen)
			if live {
				if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
					rcode := -1
					if answer != nil {
						rcode = answer.Rcode
					}
					t.Fatal("actual edge/backend answer missing", protocol, zone, rcode, err)
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

// anchorEdgeZones stores one zone per edge pool for the backend to serve.
func anchorEdgeZones(t *testing.T, cfg *config.Config) {
	t.Helper()
	st, closeStore := tsigMountedStore(t, cfg)
	defer closeStore()
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
}

// anchorEdgeOwner reads the stopped node's installed image and returns its
// managed owner version and store authority.
func anchorEdgeOwner(t *testing.T, cfg *config.Config) (accepted.Version, replication.Authority) {
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
	return owner.Version(), st.Authority()
}

// anchorEdgeBackendConfig is the shared backend setup of the live tests:
// managed anchors from one signed root fixture, edges topology, no cache.
func anchorEdgeBackendConfig(t *testing.T, cfg *config.Config, seed []string, root string) {
	t.Helper()
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled, cfg.DNSSEC.Enabled = false, false
	cfg.Cache.MaxEntries = 0
	r := &cfg.Upstreams.Recursion
	r.ManagedTopology, r.AnchorMode, r.DNSSEC = "edges", config.RootAnchorsManaged, true
	r.TrustAnchors, r.RootHints, r.IPv6 = seed, []string{root}, "off"
	// Live diagnosis only: the node's own log level, never a behavior switch.
	if level := os.Getenv("KUDURRU_ANCHOR_EDGE_LOG"); level != "" {
		cfg.Log.Level = level
	}
}

// anchorEdgeLogin logs in as the first-boot admin. A just-started writer
// refuses commands with 503 (nothing is committed) until its writable serving
// generation is current; only that refusal is retried, within a bound.
func anchorEdgeLogin(t *testing.T, client *http.Client, base string) api.SessionInfo {
	t.Helper()
	//nolint:gosec // G117: disposable loopback fixture login, never logged.
	body, err := json.Marshal(api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/auth/login", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("login transport failed", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		switch response.StatusCode {
		case http.StatusOK:
			var session api.SessionInfo
			if err := json.Unmarshal(data, &session); err != nil {
				t.Fatal(err)
			}
			return session
		case http.StatusServiceUnavailable:
			if time.Now().After(deadline) {
				t.Fatal("writer never admitted a login", string(data))
			}
			time.Sleep(50 * time.Millisecond)
		default:
			t.Fatal("login refused", response.StatusCode, string(data))
		}
	}
}

// anchorEdgePrimaryReady waits until a started writable backend serves the
// zone, admits commands and has settled its startup zone status writes, so
// later exchanges and commands do not race that cascade (D313/D332). The
// cascade is settled read-only before the first command: a command that
// lands between one of its commits and the publication that serves it is
// refused 503 with no effect, and the zone status reads below sign in once
// each (F28-EF-1 failed there 2 of 3 runs when only the first sign-in
// waited for admission).
func anchorEdgePrimaryReady(t *testing.T, cfg *config.Config) {
	t.Helper()
	awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")
	awaitAnchorEdgeBackendSettled(t, cfg)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	anchorEdgeLogin(t, &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second}, "http://"+cfg.Listeners.API.Address+api.BasePath)
	// Both zones, one per pool: each has its own startup status write.
	awaitMountedPrimarySettled(t, cfg, "edge-zone-0", "answer.anchor.test.", "192.0.2.77")
	awaitMountedPrimarySettled(t, cfg, "edge-zone-1", "answer.resolver.test.", "192.0.2.77")
}

// awaitAnchorEdgeBackendSettled waits, read-only, until the started backend's
// zone status and lifecycle commits for both primaries have landed, then for
// a direct authoritative answer from the publication that installs them.
// A first answer can precede that work by more than a second under race
// instrumentation; an authoritative answer captured from a publication that a
// later commit supersedes is withheld (D313, D332), so the one-shot edge
// queries would see no response (D383). No DNS assertion is retried.
func awaitAnchorEdgeBackendSettled(t *testing.T, cfg *config.Config) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(cfg.Node.DataDir, sqlite.FileName), RawQuery: "mode=ro"}).String()
	deadline := time.Now().Add(25 * time.Second)
	for {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal("edge backend source observation failed")
		}
		var pending, notify, heads, unavailable, unaligned int
		err = db.QueryRowContext(t.Context(), controlMountedSourceQuery).Scan(&pending, &notify, &heads, &unavailable, &unaligned)
		closeErr := db.Close()
		if err != nil || closeErr != nil {
			t.Fatal("edge backend source read/close failed")
		}
		if pending == 0 && notify == 0 && heads == 2 && unavailable == 0 && unaligned == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("edge backend source pending: statuses=%d notifications=%d heads=%d unavailable=%d unaligned=%d", pending, notify, heads, unavailable, unaligned)
		}
		time.Sleep(10 * time.Millisecond)
	}
	awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")
	awaitMountedDNS(t, cfg, "answer.resolver.test.", "192.0.2.77")
}

// anchorEdgeBootstrapReads is the most root acquisitions a fresh managed
// primary may make for its one bootstrap: the startup cascade (one status
// write per zone, the readiness login) can race a bootstrap, which is then
// acquired again. More than this is an acquisition storm, not a race.
const anchorEdgeBootstrapReads = 3

// anchorEdgeBootstrapSettled waits, bounded, until a started managed primary
// has made its bootstrap acquisitions and no raced retry is still pending,
// and returns the count. No running-node surface reports the accepted owner
// (the anchors CLI is offline), so the wait uses the updater's own pace: after
// n consecutive races the next acquisition is due within 2^(n-1) seconds,
// plus at most one second of wake delay. A quiet window longer than that
// after the last acquisition means no retry is pending; anchorEdgeOwner
// then proves the commit once the node stops.
func anchorEdgeBootstrapSettled(t *testing.T, reads *atomic.Int32) int32 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	last, quietSince := reads.Load(), time.Now()
	for {
		n := reads.Load()
		if n > anchorEdgeBootstrapReads {
			t.Fatal("managed primary made more bootstrap acquisitions than the startup cascade explains", n)
		}
		if n != last {
			last, quietSince = n, time.Now()
		}
		if n > 0 && time.Since(quietSince) > time.Second<<(n-1)+3*time.Second {
			t.Log("managed primary bootstrap acquisitions", n)
			return n
		}
		if time.Now().After(deadline) {
			t.Fatal("managed primary bootstrap acquisitions did not settle", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// This is a separately enabled actual daemon composition, not a mocked edge
// observation. Only its own pinned-image child and scratch directory are owned.
func TestRunServeManagedEdgePinnedDaemonRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	lab, _ := newAnchorEdgeLab(t, cfg)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	// The root answers every acquisition: a bootstrap that an ordinary
	// startup commit rolls back is acquired again (F28-EF-1).
	root, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
	anchorEdgeBackendConfig(t, cfg, seeds, root)
	backend := lab.backend(cfg)
	sup, stopEdge, installed := lab.start(backend)
	stopObservation := lab.observe(sup)
	cfg.Upstreams.Recursion.ManagedEdges = []config.ManagedRootEdge{lab.managedEdge(cfg, installed.Deployment)}
	anchorEdgeZones(t, cfg)
	stopBackend := tsigMountedStart(t, cfg)
	anchorEdgePrimaryReady(t, cfg)
	awaitAnchorEdgeBackendSettled(t, cfg)
	lab.wait(sup)
	lab.ask(true)
	lab.ask(true)
	// The bootstrap's acquisitions, bounded: a raced bootstrap is acquired
	// again, but only a few times.
	bootstrap := anchorEdgeBootstrapSettled(t, reads)
	stopObservation()
	stopEdge()
	next, _, after := lab.start(backend)
	if after.Deployment != installed.Deployment || after.Run == installed.Run {
		t.Fatal("edge restart lost retained deployment or fresh run identity")
	}
	lab.observe(next)
	// Child restart is protected by the deployment's immutable cache policy;
	// it cannot renew the already completed backend startup observation.
	lab.wait(next)
	lab.ask(true)
	stopBackend()
	version, _ := anchorEdgeOwner(t, cfg)
	if reads.Load() != bootstrap {
		t.Fatal("edge restart made the committed backend acquire the root again", bootstrap, reads.Load())
	}
	lab.ask(false)
	stopBackend = tsigMountedStart(t, cfg)
	anchorEdgePrimaryReady(t, cfg)
	awaitAnchorEdgeBackendSettled(t, cfg)
	lab.wait(next)
	lab.ask(true)
	stopBackend()
	if got, _ := anchorEdgeOwner(t, cfg); reads.Load() != bootstrap || got != version {
		t.Fatal("edge or backend restart bootstrapped again or advanced accepted anchors", reads.Load())
	}
}

// F28-EF-1 against the pinned dnsdist image: a primary and a newly
// joined follower are two backends of one real managed edge, following the
// runbook order "Add a follower behind managed edges".
//
//  1. The primary bootstraps managed trust behind the edge, is activated
//     through the API and restarted as the enrolled primary.
//  2. The follower's backend is added to the edge's static backends; the
//     edge child restarts with its retained bypass identity and a fresh run.
//  3. The receiver joins with the real offline `cluster join` CLI.
//  4. The follower starts with edges topology, authenticates the two-backend
//     fleet and serves both pools over UDP and TCP through the edge: with the
//     primary stopped and marked down, every answer comes from the follower.
//  5. With both backends down the edge answers nothing (no cached copy).
//     A follower restart observes the fleet again and serves again.
//
// The follower keeps the primary's accepted managed owner throughout.
func TestRunServeManagedEdgePinnedDaemonFollower(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	lab, secret := newAnchorEdgeLab(t, cfg)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	// Unlike the single-read wire fixture, this root answers every
	// acquisition: the primary's bootstrap and each follower supervisor's.
	root, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
	anchorEdgeBackendConfig(t, cfg, seeds, root)
	cfg.Node.Name = "edge-primary"
	primary := lab.backend(cfg)
	sup, stopEdge, installed := lab.start(primary)
	stopObservation := lab.observe(sup)
	cfg.Upstreams.Recursion.ManagedEdges = []config.ManagedRootEdge{lab.managedEdge(cfg, installed.Deployment)}
	anchorEdgeZones(t, cfg)
	stopPrimary := tsigMountedStart(t, cfg)
	anchorEdgePrimaryReady(t, cfg)
	awaitAnchorEdgeBackendSettled(t, cfg)
	lab.wait(sup)
	lab.ask(true)
	// Settle the bootstrap (bounded) before the activation restart and the
	// join read the primary's managed owner.
	bootstrap := anchorEdgeBootstrapSettled(t, reads)

	// Enrollment goes through the primary's mounted API, as an operator does.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	session := anchorEdgeLogin(t, client, base)
	review := func() api.ClusterEnrollmentInfo {
		t.Helper()
		var info api.ClusterEnrollmentInfo
		staticMountedRequest(t, client, base, "GET", "/cluster/enrollment", "", nil, 200, &info)
		return info
	}
	original := review()
	if !original.Dormant || original.Mode != "standalone" {
		t.Fatal("standalone primary did not expose dormant enrollment", original.Mode)
	}
	var activated api.ClusterActivationReceipt
	staticMountedRequest(t, client, base, "POST", "/cluster/activate", session.CSRF, api.ActivateClusterCommand{ClusterEnrollmentReview: original.ClusterEnrollmentReview,
		Name: cfg.Node.Name, Address: cfg.Listeners.Internal.Address, Reason: "live edge follower acceptance"}, 200, &activated)
	if !activated.Committed || !activated.RestartRequired {
		t.Fatal("activation did not commit")
	}
	stopPrimary()
	stopPrimary = tsigMountedStart(t, cfg)
	anchorEdgePrimaryReady(t, cfg)
	awaitAnchorEdgeBackendSettled(t, cfg)
	lab.states(sup, map[string]bool{primary.Address: true}) // edge health saw the restart
	lab.ask(true)
	// A fresh session on the restarted primary (a logged-in client must send
	// CSRF even to log in again).
	if client.Jar, err = cookiejar.New(nil); err != nil {
		t.Fatal(err)
	}
	session = anchorEdgeLogin(t, client, base)
	enrolled := review()
	if enrolled.Dormant || enrolled.Mode != "primary" {
		t.Fatal("restart did not mount the enrolled primary", enrolled.Mode)
	}

	// The follower host: its own listeners, the edge's PROXYv2 secret and
	// internal credentials, and the same managed anchor host settings.
	fcfg, _ := mountedTestConfig(t)
	anchorEdgeBackendConfig(t, fcfg, seeds, root)
	fcfg.Node.Name = "edge-follower"
	fcfg.Node.DataDir = filepath.Join(t.TempDir(), "edge-follower") // the join creates it
	fcfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	fcfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	fcfg.Listeners.ProxyProtocolSecretFile = secret
	follower := lab.backend(fcfg)

	var invitation api.ClusterJoinTokenReceipt
	staticMountedRequest(t, client, base, "POST", "/cluster/join-token", session.CSRF, api.ClusterJoinTokenCommand{ClusterEnrollmentReview: enrolled.ClusterEnrollmentReview,
		Name: fcfg.Node.Name, Address: fcfg.Listeners.Internal.Address, ExpiresAt: time.Now().Add(2 * time.Minute), Reason: "join live edge follower"}, 200, &invitation)
	if !invitation.Committed || invitation.Invitation == "" {
		t.Fatal("invitation not committed")
	}

	// Runbook step 1: every edge lists every node. The edge child restarts
	// with its retained bypass identity before the follower exists.
	stopObservation()
	stopEdge()
	sup, stopEdge, added := lab.start(primary, follower)
	if added.Deployment != installed.Deployment || added.Run == installed.Run || len(added.Backends) != 2 {
		t.Fatal("backend addition lost the retained deployment, run identity or membership", len(added.Backends))
	}
	lab.observe(sup) // its cleanup stops this listener
	lab.states(sup, map[string]bool{primary.Address: true, follower.Address: false})
	lab.ask(true)

	// Runbook step 2: the real offline join installs the first image.
	file := filepath.Join(t.TempDir(), "invitation")
	if err := os.WriteFile(file, []byte(invitation.Invitation), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	join := newClusterCmd(&cliOpts{})
	join.SetOut(&output)
	join.SetArgs([]string{"join", "--offline", "--data-dir", fcfg.Node.DataDir, "--invitation-file", file, "--confirm-name", fcfg.Node.Name, "--confirm-address", fcfg.Listeners.Internal.Address})
	if err := join.ExecuteContext(t.Context()); err != nil {
		t.Fatal("offline join failed", err)
	}
	var joined clusterreplica.JoinResult
	if err := json.Unmarshal(output.Bytes(), &joined); err != nil || !joined.Primary.Committed || joined.Installed.Validate() != nil {
		t.Fatal("offline join did not commit and install", err)
	}
	version, authority := anchorEdgeOwner(t, fcfg)
	// The first image carries the primary's committed managed owner, so the
	// primary's acquisitions are done (its next refresh is far away): the
	// activation restart acquired nothing.
	if reads.Load() != bootstrap {
		t.Fatal("enrolled primary restart acquired the root again", bootstrap, reads.Load())
	}
	if authority.Mode != replication.Follower || !authority.Installed.Same(joined.Installed) {
		t.Fatal("joined receiver is not an installed follower", authority.Mode)
	}

	// Runbook steps 3 and 4: the follower declares its own backend on the
	// edge and starts behind it.
	fcfg.Upstreams.Recursion.ManagedEdges = []config.ManagedRootEdge{lab.managedEdge(fcfg, installed.Deployment)}
	observed := lab.observations.Load()
	stopFollower := tsigMountedStart(t, fcfg)
	awaitMountedDNS(t, fcfg, "answer.anchor.test.", "192.0.2.77")
	awaitAnchorEdgeBackendSettled(t, fcfg)
	if got := lab.observations.Load() - observed; got != 1 {
		t.Fatal("follower start did not observe the one-edge fleet exactly once", got)
	}
	lab.states(sup, map[string]bool{primary.Address: true, follower.Address: true})
	lab.ask(true)
	// The deny-only supervisor runs after startup; wait (bounded) for its own
	// acquisition.
	until := time.Now().Add(15 * time.Second)
	for reads.Load() <= bootstrap {
		if time.Now().After(until) {
			t.Fatal("follower supervisor made no root acquisition of its own", reads.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	afterStart := reads.Load()

	// Only the follower remains: both pools answer from it alone.
	stopPrimary()
	lab.states(sup, map[string]bool{primary.Address: false, follower.Address: true})
	lab.ask(true)
	lab.ask(true)

	// The edge child restarts again while the joined follower serves alone:
	// the fresh run identity does not disturb the admitted follower, which
	// keeps serving and does not observe the fleet again. The observation
	// listener stays up across the restart, so an observation attempted
	// while the child is down is counted (and refused) too.
	lab.edge.Store(nil)
	stopEdge()
	sup, _, rerun := lab.start(primary, follower) // its cleanup stops this edge
	if rerun.Deployment != installed.Deployment || rerun.Run == added.Run || len(rerun.Backends) != 2 {
		t.Fatal("edge restart under the serving follower lost the retained deployment, run identity or membership", len(rerun.Backends))
	}
	lab.edge.Store(sup)
	lab.states(sup, map[string]bool{primary.Address: false, follower.Address: true})
	lab.ask(true)
	if got := lab.observations.Load() - observed; got != 1 {
		t.Fatal("serving follower observed the fleet again after its start", got)
	}

	// Runbook step 5: with no backend up the edge returns no answer.
	stopFollower()
	lab.states(sup, map[string]bool{primary.Address: false, follower.Address: false})
	lab.ask(false)
	if got, _ := anchorEdgeOwner(t, fcfg); got != version {
		t.Fatal("follower changed its accepted managed owner", got)
	}
	observed = lab.observations.Load()
	stopFollower = tsigMountedStart(t, fcfg)
	awaitMountedDNS(t, fcfg, "answer.anchor.test.", "192.0.2.77")
	awaitAnchorEdgeBackendSettled(t, fcfg)
	if got := lab.observations.Load() - observed; got != 1 {
		t.Fatal("follower restart did not observe the fleet again exactly once", got)
	}
	lab.states(sup, map[string]bool{primary.Address: false, follower.Address: true})
	lab.ask(true)
	if got := lab.observations.Load() - observed; got != 1 {
		t.Fatal("restarted follower observed the fleet again while serving", got)
	}
	stopFollower()
	got, restarted := anchorEdgeOwner(t, fcfg)
	if got != version || restarted.Mode != replication.Follower || !restarted.Installed.Same(joined.Installed) {
		t.Fatal("follower restart changed its owner or authority", restarted.Mode)
	}
	// The restarted deny-only supervisor may make at most one more acquisition.
	if total := reads.Load(); total > afterStart+1 {
		t.Fatal("follower restart reacquired the root more than once", afterStart, total)
	}
	primaryVersion, _ := anchorEdgeOwner(t, cfg)
	if primaryVersion != version {
		t.Fatal("follower serves a different managed owner than its primary")
	}
}
