package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	var version accepted.Version
	for phase := range 3 {
		t.Logf("managed anchor lifecycle phase=%d", phase)
		if phase == 2 {
			cfg.Upstreams.Recursion.ManagedTopology = "disabled"
		}
		stop := tsigMountedStart(t, cfg)
		switch phase {
		case 0:
			awaitMountedPrimaryCreated(t, cfg, anchorMountedObjects(), "anchor", "answer.anchor.test.", "192.0.2.77")
		case 1:
			awaitMountedPrimarySettled(t, cfg, "anchor", "answer.anchor.test.", "192.0.2.77")
		default:
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

// anchorMountedObjects are the zone and record the managed anchor mounts
// serve, as documents of one apply request.
func anchorMountedObjects() []any {
	return []any{
		&public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "anchor"},
			Spec: public.DNSZoneSpec{ZoneName: "anchor.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.anchor.test."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: "answer", Namespace: "default"},
			Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "anchor"}, Endpoints: []public.Endpoint{{DNSName: "answer.anchor.test.", RecordType: "A", Targets: []string{"192.0.2.77"}}}}},
	}
}

// mountedAnchorSession logs in once to a mounted node. A login is a command
// held to the publication its request began on: any publication before its
// commit refuses it with 503 and no effect (D313), and it is never replayed.
// Callers therefore log in only where no publication of the node's own
// startup is still due.
func mountedAnchorSession(t *testing.T, cfg *config.Config) (*http.Client, string, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var session api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &session)
	return client, base, session.CSRF
}

// awaitMountedPrimaryCreated creates the objects of a first start on a node
// that holds none and waits until they are settled. A zone written to the
// store before the first start has no status yet: its first Ready status
// write lands about half a second after the node first answers it, and a
// login made in that window was refused (hosted CI on 7648c479). Here the
// login runs on the ready node before any zone exists, so no startup
// publication is due, and every later wait is an observation.
func awaitMountedPrimaryCreated(t *testing.T, cfg *config.Config, objects []any, zoneObject, name, address string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("mounted primary never became ready")
		}
		time.Sleep(25 * time.Millisecond)
	}
	client, base, csrf := mountedAnchorSession(t, cfg)
	var applied api.List[api.ApplyResult]
	controlMountedRequest(t, client, base, http.MethodPost, "/apply", csrf, objects, http.StatusOK, &applied)
	if len(applied.Items) != len(objects) {
		t.Fatal("apply lost a per-document result", len(applied.Items))
	}
	for _, item := range applied.Items {
		if item.Result != api.ResultCreated {
			t.Fatalf("apply did not create %s/%s: %s %s", item.Kind, item.Name, item.Result, item.ErrorKey)
		}
	}
	awaitMountedZoneSettled(t, cfg, client, base, zoneObject, name, address)
}

// awaitMountedPrimarySettled is the wait of a restart: the zone already has
// its Ready status, so a restart publishes no status write and the login
// after the first answer meets no startup publication.
func awaitMountedPrimarySettled(t *testing.T, cfg *config.Config, zoneObject, name, address string) {
	t.Helper()
	awaitMountedDNS(t, cfg, name, address)
	client, base, _ := mountedAnchorSession(t, cfg)
	awaitMountedZoneSettled(t, cfg, client, base, zoneObject, name, address)
}

// awaitMountedZoneSettled waits until the zone's own status reconciliation
// has published Ready for the current generation AND the driver has realigned
// its source, so later exchanges do not race the status write. A status-only
// source gap deliberately makes the zone unavailable until that alignment,
// and retired in-flight answers are dropped (D313/D332). Reads are
// observations: one refused because a publication landed while it ran (503)
// is read again. The wait only orders the test after that cascade; every
// exchange assertion is unchanged.
func awaitMountedZoneSettled(t *testing.T, cfg *config.Config, client *http.Client, base, zoneObject, name, address string) {
	t.Helper()
	read := func() (string, bool) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/objects/dnszones/"+zoneObject, nil)
		if err != nil {
			t.Fatal("zone observation construction failed")
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("zone observation transport failed")
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		switch {
		case err == nil && response.StatusCode == http.StatusServiceUnavailable:
			return "", false
		case err != nil || response.StatusCode != http.StatusOK:
			t.Fatalf("zone observation: status=%d body_read=%t", response.StatusCode, err == nil)
		}
		var zone public.DNSZone
		if json.Unmarshal(data, &zone) != nil {
			t.Fatal("zone observation decode failed")
		}
		ready := meta.FindStatusCondition(zone.Status.Conditions, public.ConditionReady)
		return zone.ResourceVersion, ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == zone.Generation
	}
	deadline := time.Now().Add(time.Minute)
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
