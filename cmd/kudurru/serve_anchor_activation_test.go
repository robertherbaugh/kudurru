package main

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
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
	read := func() (string, bool) {
		var zone public.DNSZone
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/dnszones/"+zoneObject, "", nil, http.StatusOK, &zone)
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
