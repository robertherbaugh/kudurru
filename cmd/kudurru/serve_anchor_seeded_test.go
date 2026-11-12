package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRunServeDirectAnchorsSeededFirstStartRefusesStraddlingLogin starts a
// managed direct anchor node for the first time on a store that already
// holds a zone without status, as a restored or imported store does. The
// node bootstraps its anchors once and serves the zone.
//
// It also pins what a sign-in meets when another commit publishes while it
// runs (hosted CI on 7648c479 met the zone's startup status write). A login
// is a command held to the publication its request began on (D199, D208):
// its headers arrive, a signed UPDATE then commits and is served, and only
// then does its body arrive. The login is refused with 503 unavailable and
// Retry-After, sets no cookie, and is not replayed.
func TestRunServeDirectAnchorsSeededFirstStartRefusesStraddlingLogin(t *testing.T) {
	cfg, secret := mountedTestConfig(t)
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
	files, _ := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "anchor"}, Spec: public.DNSZoneSpec{ZoneName: "anchor.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.anchor.test."},
			Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.anchor.test.", Types: []string{"A"}}}}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "answer", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "anchor"}, Endpoints: []public.Endpoint{{DNSName: "answer.anchor.test.", RecordType: "A", Targets: []string{"192.0.2.77"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	defer stop()
	awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")

	// The login's headers are on the wire once the first body byte is
	// requested; the request captures its publication then.
	reader, writer := io.Pipe()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+cfg.Listeners.API.Address+api.BasePath+"/auth/login", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	type result struct {
		status            int
		key, retry        string
		cookies, readBody bool
	}
	done := make(chan result, 1)
	go func() {
		client := &http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}
		response, err := client.Do(req)
		if err != nil {
			done <- result{}
			return
		}
		defer func() { _ = response.Body.Close() }()
		var refusal struct {
			Error struct{ Key string } `json:"error"`
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		decoded := err == nil && json.Unmarshal(data, &refusal) == nil
		done <- result{status: response.StatusCode, key: refusal.Error.Key, retry: response.Header.Get("Retry-After"), cookies: len(response.Cookies()) != 0, readBody: decoded}
	}()
	if _, err := writer.Write([]byte(`{"username":"admin",`)); err != nil {
		t.Fatal(err)
	}

	// Another commit on the node: a signed UPDATE. SERVFAIL means it was not
	// applied while the node republished (docs/deploy/dynamic-updates.md),
	// and an RFC 2136 client sends it again; any other refusal fails.
	update := func() int {
		m := new(dns.Msg)
		m.SetUpdate("anchor.test.")
		m.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "during-login.anchor.test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 78)}})
		m.SetTsig("updates.", dns.HmacSHA256, 300, time.Now().Unix())
		answer, _, err := (&dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigSecret: map[string]string{"updates.": secret}}).Exchange(m, cfg.Listeners.DNS[0].Address)
		if err != nil || answer == nil {
			return -1
		}
		return answer.Rcode
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		rcode := update()
		if rcode == dns.RcodeSuccess {
			break
		}
		if rcode != dns.RcodeServerFailure && rcode != -1 {
			t.Fatal("signed UPDATE during the login was refused", dns.RcodeToString[rcode])
		}
		if time.Now().After(deadline) {
			t.Fatal("signed UPDATE during the login never committed")
		}
	}
	// The node serves the UPDATE's publication before the login commits.
	awaitMountedDNS(t, cfg, "during-login.anchor.test.", "192.0.2.78")
	if _, err := writer.Write([]byte(`"password":"mounted-test-password-1"}`)); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	got := <-done
	if got.status != http.StatusServiceUnavailable || !got.readBody || got.key != "unavailable" || got.retry == "" {
		t.Fatalf("login across another commit: status=%d key=%q retry-after=%q, want 503 unavailable with Retry-After", got.status, got.key, got.retry)
	}
	if got.cookies {
		t.Fatal("a refused login set a cookie")
	}
	awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")
	stop()
	if reads.Load() != 1 {
		t.Fatal("seeded first start did not bootstrap managed trust exactly once", reads.Load())
	}
	reopened, closeAgain := tsigMountedStore(t, cfg)
	defer closeAgain()
	image, err := reopened.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Inspect().Phase != accepted.Managed {
		t.Fatal("seeded first start did not commit managed history")
	}
}
