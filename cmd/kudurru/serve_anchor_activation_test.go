package main

import (
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
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
		if phase == 2 {
			cfg.Upstreams.Recursion.ManagedTopology = "disabled"
		}
		stop := tsigMountedStart(t, cfg)
		if phase < 2 {
			awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")
		} else {
			tsigMountedAwaitPendingListeners(t, cfg)
		}
		for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
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
