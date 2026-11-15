package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Both nodes are actual runServe assemblies. The primary bootstraps from a
// signed TCP response; the follower independently observes a signed REVOKE.
// The fixture performs only an explicit complete-image enrollment/install while
// both servers are stopped. It supplies no scheduler step or forged clock.
func TestRunServeDirectAnchorFollowerRevocationTransportsAndRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	endpoint, seeds, reads := anchorOperationServer(t, 2, func(n int) {
		if n == 2 {
			close(entered)
			<-release
		}
	})
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.DNSSEC = true
	cfg.Upstreams.Recursion.TrustAnchors = seeds
	cfg.Upstreams.Recursion.RootHints = []string{endpoint}
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
	stopPrimary := tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "answer.anchor.test.", "192.0.2.77")
	stopPrimary()
	primary, closePrimary := tsigMountedStore(t, cfg)
	followerConfig, err := config.FromFlat(config.Flatten(cfg))
	if err != nil {
		t.Fatal(err)
	}
	followerConfig.Node.DataDir, followerConfig.Node.Name = t.TempDir(), "anchor-follower"
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	options := sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: followerConfig.Node.Name,
		ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage}
	path := filepath.Join(followerConfig.Node.DataDir, sqlite.FileName)
	follower, err := sqlite.Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = follower.Close() })
	original, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := follower.Enroll(original, primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	// The actual target capture precedes the complete source acquisition.
	install := captureServingReplica(t, follower)
	expected := follower.Authority().Installed
	image, err := primary.ReadGeneration(install)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(install, *image)
	if err != nil {
		t.Fatal(err)
	}
	if installed, err := follower.InstallReplica(install, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: expected}); err != nil || !installed.Same(image.Generation()) {
		t.Fatal("original follower install", err)
	}
	before := anchorFollowerImage(original, t, follower)
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	closePrimary()
	checkAll := func(want int) {
		for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
			query := new(dns.Msg)
			query.SetQuestion("answer.anchor.test.", dns.TypeA)
			query.Id = 0
			raw, err := query.Pack()
			if err != nil {
				t.Fatal(err)
			}
			answer := new(dns.Msg)
			if err := answer.Unpack(tsigMountedExchange(t, followerConfig, roots, protocol, raw)); err != nil {
				t.Fatal(err)
			}
			if answer.Rcode != want {
				t.Fatal("follower output disposition", protocol, answer.Rcode, want)
			}
			if want == dns.RcodeSuccess {
				if len(answer.Answer) != 1 || !answer.Authoritative {
					t.Fatal("follower did not serve installed authoritative image", protocol)
				}
				if a, ok := answer.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.77" {
					t.Fatal("follower changed original installed answer")
				}
			} else if len(answer.Answer) != 0 || len(answer.Ns) != 0 || answer.IsTsig() != nil || answer.AuthenticatedData {
				t.Fatal("retired follower emitted old data or proof", protocol)
			}
		}
	}
	for phase := range 2 {
		stop := tsigMountedStart(t, followerConfig)
		if phase == 0 {
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("runServe follower did not independently acquire root evidence")
			}
			awaitMountedDNS(t, followerConfig, "answer.anchor.test.", "192.0.2.77")
			checkAll(dns.RcodeSuccess)
			unblock()
		}
		tsigMountedAwaitPendingListeners(t, followerConfig)
		checkAll(dns.RcodeServerFailure)
		stop()
		follower, err = sqlite.Open(path, options)
		if err != nil {
			t.Fatal(err)
		}
		if !anchorFollowerSameImage(before, anchorFollowerImage(t.Context(), t, follower)) || reads.Load() != 2 {
			t.Fatal("follower denial/restart changed accepted state or reacquired", phase, reads.Load())
		}
		if err := follower.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
