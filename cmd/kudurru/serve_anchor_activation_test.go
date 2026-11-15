package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The actual root assembly must hold its legacy elected writers as well as
// its primary driver while the first root proof is in flight. A controller
// status write used to roll this acquisition back and force another one.
func TestRunServeManagedAnchorFirstAcquisitionPrecedesLeaderWrites(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	seed, reply := anchorWireSignedReply(t)
	requested, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var first sync.Once
	endpoint, reads := anchorWireServer(t, "tcp", func(raw []byte) []byte {
		first.Do(func() { close(requested); <-release })
		return reply(raw)
	})
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.DNSSEC = true
	cfg.Upstreams.Recursion.TrustAnchors = seed.DS
	cfg.Upstreams.Recursion.RootHints = []string{endpoint.String()}
	cfg.Upstreams.Recursion.IPv6 = "off"
	st, closeStore := tsigMountedStore(t, cfg)
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "anchor"}, Spec: public.DNSZoneSpec{ZoneName: "anchor.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.anchor.test."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}}
	if _, err := st.Apply(t.Context(), zone, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	record := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "answer", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "anchor"}, Endpoints: []public.Endpoint{{DNSName: "answer.anchor.test.", RecordType: "A", Targets: []string{"192.0.2.77"}}}}}
	if _, err := st.Apply(t.Context(), record, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	select {
	case <-requested:
	case <-time.After(10 * time.Second):
		t.Fatal("managed bootstrap did not acquire")
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(cfg.Node.DataDir, sqlite.FileName), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	read := func() string {
		t.Helper()
		var authority string
		if err := db.QueryRowContext(t.Context(), "SELECT value FROM meta WHERE key='replication_authority'").Scan(&authority); err != nil {
			t.Fatal(err)
		}
		return authority
	}
	before := read()
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if read() != before {
			t.Fatal("a startup writer committed while the first managed acquisition was in flight")
		}
	}
	unblock()
	awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")
	stop()
	if reads.Load() != 1 {
		t.Fatal("bootstrap acquired more than once", reads.Load())
	}
}

func TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.DNSSEC = true
	cfg.Upstreams.Recursion.TrustAnchors = seed.DS
	cfg.Upstreams.Recursion.RootHints = []string{endpoint.String()}
	cfg.Upstreams.Recursion.IPv6 = "off"
	files, roots := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "anchor"}, Spec: public.DNSZoneSpec{ZoneName: "anchor.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.anchor.test."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "answer", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "anchor"}, Endpoints: []public.Endpoint{{DNSName: "answer.anchor.test.", RecordType: "A", Targets: []string{"192.0.2.77"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore()
	var version accepted.Version
	for phase := range 3 {
		t.Logf("managed anchor lifecycle phase=%d", phase)
		if phase == 2 {
			cfg.Upstreams.Recursion.ManagedTopology = "disabled"
		}
		stop := tsigMountedStart(t, cfg)
		if phase < 2 {
			awaitMountedPrimarySettled(t, cfg, "anchor", "answer.anchor.test.", "192.0.2.77")
		} else {
			tsigMountedAwaitPendingListeners(t, cfg)
		}
		for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
			t.Logf("managed anchor exchange phase=%d protocol=%s", phase, protocol)
			query := new(dns.Msg)
			query.SetQuestion("answer.anchor.test.", dns.TypeA)
			query.Id = 0 // DoQ requires the DNS message ID to be zero.
			raw, err := query.Pack()
			if err != nil {
				t.Fatal(err)
			}
			answer := new(dns.Msg)
			if err := answer.Unpack(tsigMountedExchange(t, cfg, roots, protocol, raw)); err != nil {
				t.Fatal(err)
			}
			if phase < 2 {
				if answer.Rcode != dns.RcodeSuccess || !answer.Authoritative || len(answer.Answer) != 1 {
					t.Fatal("managed native output unavailable", phase, protocol, answer.Rcode)
				}
				if a, ok := answer.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.77" {
					t.Fatal("managed output changed durable answer")
				}
			} else if answer.Rcode != dns.RcodeServerFailure || len(answer.Answer) != 0 || len(answer.Ns) != 0 || answer.IsTsig() != nil {
				t.Fatal("disabled managed admission emitted data", protocol)
			}
		}
		stop()
		if reads.Load() != 1 {
			t.Fatal("restart reset accepted refresh or reacquired root proof", phase, reads.Load())
		}
		reopened, closeAgain := tsigMountedStore(t, cfg)
		image, err := reopened.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		if owner.Inspect().Phase != accepted.Managed {
			t.Fatal("managed history missing after actual restart")
		}
		if phase == 0 {
			version = owner.Version()
		} else if owner.Version() != version {
			t.Fatal("restart or disabled topology changed accepted checkpoint")
		}
		closeAgain()
	}
}

// awaitMountedPrimarySettled waits until the zone's own status reconciliation
// has published Ready for the current generation AND the driver has realigned
// its source, so later exchanges do not race the startup status write. A
// status-only source gap keeps serving the committed head, but each of these
// commits retires in-flight answers, which are dropped (D313/D332). The wait
// only orders the test after that cascade; every exchange assertion is unchanged.
func awaitMountedPrimarySettled(t *testing.T, cfg *config.Config, zoneObject, name, address string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	awaitMountedDNS(t, cfg, name, address)
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, nil)
	deadline := time.Now().Add(time.Minute)
	// read is one GET of the zone. The node's own status commits are what
	// this wait orders the test after, so the GET may arrive while their
	// newest generation is still being prepared and be refused at capture
	// (503 generation_pending with Retry-After, before the request was
	// read). It is asked again after Retry-After within the same one-minute
	// bound; any other answer than 200 fails the test as before.
	read := func() (string, bool) {
		var zone public.DNSZone
		for {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/objects/dnszones/"+zoneObject, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatal("control HTTP transport failed", err)
			}
			raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
			_ = res.Body.Close()
			if err == nil && res.StatusCode == http.StatusOK {
				if json.Unmarshal(raw, &zone) != nil {
					t.Fatal("control HTTP response decode failed")
				}
				break
			}
			retry, _ := strconv.Atoi(res.Header.Get("Retry-After"))
			if err != nil || res.StatusCode != http.StatusServiceUnavailable || retry < 1 || !bytes.Contains(raw, []byte(`"generation_pending"`)) || time.Now().After(deadline) {
				t.Fatalf("control GET /objects/dnszones/%s: status=%d want=200: %s", zoneObject, res.StatusCode, raw)
			}
			time.Sleep(time.Duration(retry) * time.Second)
		}
		ready := meta.FindStatusCondition(zone.Status.Conditions, public.ConditionReady)
		return zone.ResourceVersion, ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == zone.Generation
	}
	for {
		before, ready := read()
		if ready {
			awaitMountedDNS(t, cfg, name, address)
			if after, still := read(); still && after == before {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("mounted primary zone status never settled", zoneObject)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
