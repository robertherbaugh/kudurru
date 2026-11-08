package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	stored "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
	"github.com/robertherbaugh/kudurru/internal/zone"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestRunServeKubeStoredTSIGStartupTransportsRestartAndOffline(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	actual, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(actual, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var namespace corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &namespace); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(actual.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport, err = rest.TransportFor(actual)
	if err != nil {
		t.Fatal(err)
	}
	var unavailable atomic.Bool
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.Listeners.DNS[0].Protocols = []string{"udp", "tcp"}
	files, roots := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	fileKey := dnsserver.TSIGKey{Name: "file.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{0x62}, 32)}
	filePath := filepath.Join(t.TempDir(), "file-tsig")
	if err := os.WriteFile(filePath, []byte(base64.StdEncoding.EncodeToString(fileKey.Secret)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.TSIGKeys = []config.TSIGKey{{Name: fileKey.Name, Algorithm: fileKey.Algorithm, SecretFile: filePath}}
	st, err := kube.New(actual, kube.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), ExpectedClusterID: string(namespace.UID), StartupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seal, err := tsigkeys.NewSealer(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	material, err := tsigkeys.NewMaterial(tsigkeys.Owner{ClusterID: string(namespace.UID), UID: "provisioning-only", KeyName: "stored.", Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	provisional, err := seal.Seal(material)
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.Apply(t.Context(), provisional, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
	if err != nil {
		t.Fatal("create actual private carrier", err)
	}
	owner := tsigkeys.Owner{ClusterID: string(namespace.UID), UID: string(created.GetUID()), KeyName: "stored.", Algorithm: dns.HmacSHA256}
	material, err = tsigkeys.NewMaterial(owner, bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	row, err := seal.Seal(material)
	if err != nil {
		t.Fatal(err)
	}
	created, err = st.Apply(t.Context(), row, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: created.GetResourceVersion()})
	if err != nil {
		t.Fatal("bind actual Secret UID", err)
	}
	primary := tsigMountedPrimary(t, material, "tcp", config.TLSFiles{})
	notifyAddress, notifications := kubeTSIGNotifyTarget(t, material)
	var secondary *public.DNSZone
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "signed"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "signed.test.", Nameservers: []string{"ns.signed.test."}, Policy: public.ZonePolicy{Notify: public.NotifySpecified, NotifyTargets: []string{notifyAddress}, NotifyTSIGKeyRef: "stored."}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "signed"}, Endpoints: []public.Endpoint{{DNSName: "seed.signed.test.", RecordType: "A", Targets: []string{"192.0.2.91"}}}}},
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "acquired"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeSecondary, ZoneName: "acquired.test.", Secondary: &public.SecondarySpec{Primaries: []string{primary.address}, TransferProtocol: "tcp", TSIGKeyRef: "stored."}}},
	} {
		value, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI})
		if err != nil {
			t.Fatal(err)
		}
		if dz, ok := value.(*public.DNSZone); ok && dz.Spec.Type == public.ZoneTypeSecondary {
			secondary = dz
		}
	}
	var originalFingerprint string
	var artifact tsigMountedArtifact
	for phase := 0; phase < 4; phase++ {
		if phase == 2 {
			// A still-valid file key cannot certify an unknown private inventory.
			unavailable.Store(true)
			cfg.Store.Kube.StartupTimeout = config.Duration(100 * time.Millisecond)
		}
		if phase == 3 {
			unavailable.Store(false)
			cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
			// A complete carrier with wrong encrypted owner rejects the whole
			// source. No reader repairs it or silently falls back to the file key.
			bad, err := seal.Seal(mustKubeTSIGMaterial(t, tsigkeys.Owner{ClusterID: owner.ClusterID, UID: "foreign-carrier", KeyName: owner.KeyName, Algorithm: owner.Algorithm}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Apply(t.Context(), bad, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: created.GetResourceVersion()}); err != nil {
				t.Fatal(err)
			}
		}
		beforeNotify, beforeSOA := notifications.Load(), primary.soa.Load()
		stop := startStaticMount(t, cfg)
		awaitMountedDNS(t, cfg, "seed.signed.test.", "192.0.2.91")
		if phase < 2 {
			awaitMountedDNS(t, cfg, "host.acquired.test.", "192.0.2.92")
			deadline := time.Now().Add(10 * time.Second)
			for notifications.Load() <= beforeNotify || primary.soa.Load() <= beforeSOA {
				if time.Now().After(deadline) {
					t.Fatal("mounted immutable source did not authenticate actual NOTIFY/SOA")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if primary.secondary.Load() == 0 {
				t.Fatal("mounted factory did not perform actual signed transfer")
			}
		} else {
			tsigMountedAwaitRcode(t, cfg, "host.acquired.test.", dns.RcodeServerFailure, true)
		}
		for _, key := range []dnsserver.TSIGKey{material.Key(), fileKey} {
			keys, err := dnsserver.NewStaticKeyring(key)
			if err != nil {
				t.Fatal(err)
			}
			for _, proto := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
				q := new(dns.Msg).SetQuestion("seed.signed.test.", dns.TypeA)
				q.Id = 0
				q.SetTsig(key.Name, key.Algorithm, 30, time.Now().Unix())
				wire, mac, err := dns.TsigGenerateWithProvider(q, dnsserver.TSIGProvider(keys), "", false)
				if err != nil {
					t.Fatal(err)
				}
				out := tsigMountedExchange(t, cfg, roots, proto, wire)
				answer := new(dns.Msg)
				if err := answer.Unpack(out); err != nil {
					t.Fatal(err)
				}
				if phase < 2 {
					if answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 || answer.IsTsig() == nil || dns.TsigVerifyWithProvider(bytes.Clone(out), dnsserver.TSIGProvider(keys), mac, false) != nil {
						t.Fatalf("phase %d %s lost actual stored/file signature", phase, proto)
					}
				} else if answer.Rcode != dns.RcodeServerFailure || answer.IsTsig() != nil || len(answer.Answer) != 0 {
					t.Fatalf("phase %d %s admitted an incomplete source", phase, proto)
				}
			}
		}
		if phase < 2 {
			startup, err := newKubeTSIGStartup(t.Context(), st, seal, mustKubeTSIGRing(t, fileKey), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			awaitStartupTSIG(t, startup)
			dz := &public.DNSZone{Spec: public.DNSZoneSpec{Secondary: &public.SecondarySpec{TSIGKeyRef: owner.KeyName}}}
			cut := startup.captureAcquisition(dz)
			if cut == nil {
				t.Fatal("actual source omitted acquisition")
			}
			fp, ok := dnsserver.AcquisitionFingerprint(cut.Keys(), owner.KeyName)
			if !ok || len(fp) < 10 || fp[:10] != "stored/v1/" {
				t.Fatal("stored epoch missing from mounted acquisition")
			}
			if phase == 0 {
				originalFingerprint = fp
			} else if fp != originalFingerprint {
				t.Fatal("restart changed same credential provenance")
			}
			startup.Close()
		}
		stop()
		got := kubeTSIGAcquiredArtifact(t, cfg, secondary)
		if phase < 2 {
			if got.Credential != originalFingerprint || got.ConfirmedAt.IsZero() || !got.ExpiresAt.After(got.ConfirmedAt) {
				t.Fatal("actual acquired data lost stored credential provenance")
			}
			artifact = got
		} else if got != artifact || primary.soa.Load() != beforeSOA || notifications.Load() != beforeNotify {
			t.Fatal("unavailable private source renewed acquired data or dispatched signed work")
		}
	}
	// No setup test grants Kubernetes hot mutation authority.
	if created.GetObjectKind().GroupVersionKind().Kind != stored.KindTSIGKey {
		t.Fatal("fixture did not create actual private kind")
	}
}

func kubeTSIGAcquiredArtifact(t *testing.T, cfg *config.Config, secondary *public.DNSZone) tsigMountedArtifact {
	t.Helper()
	db, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(cfg.Node.DataDir, localdb.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	blob, err := db.GetBlob(t.Context(), "secondary/"+zone.SecondaryIdentity(secondary))
	var result tsigMountedArtifact
	if err != nil || json.Unmarshal(blob.Data, &result) != nil {
		t.Fatal("read original acquired envelope")
	}
	return result
}

func kubeTSIGNotifyTarget(t *testing.T, material *tsigkeys.Material) (string, *atomic.Int64) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ring := mustKubeTSIGRing(t, material.Key())
	count := new(atomic.Int64)
	started, done := make(chan struct{}), make(chan error, 1)
	server := &dns.Server{PacketConn: pc, TsigProvider: dnsserver.TSIGProvider(ring), NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		if q.Opcode != dns.OpcodeNotify || q.IsTsig() == nil || w.TsigStatus() != nil {
			t.Error("mounted NOTIFY lost original stored signature")
			return
		}
		count.Add(1)
		answer := new(dns.Msg).SetReply(q)
		answer.SetTsig(q.IsTsig().Hdr.Name, q.IsTsig().Algorithm, 30, time.Now().Unix())
		if err := w.WriteMsg(answer); err != nil {
			t.Error("signed NOTIFY reply failed")
		}
	})}
	go func() { done <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.ShutdownContext(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("NOTIFY fixture did not join")
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("NOTIFY fixture did not start")
	}
	return pc.LocalAddr().String(), count
}

func mustKubeTSIGMaterial(t *testing.T, owner tsigkeys.Owner) *tsigkeys.Material {
	t.Helper()
	material, err := tsigkeys.NewMaterial(owner, bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return material
}
func mustKubeTSIGRing(t *testing.T, key dnsserver.TSIGKey) *dnsserver.StaticKeyring {
	t.Helper()
	ring, err := dnsserver.NewStaticKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}
