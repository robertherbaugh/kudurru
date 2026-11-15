package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

func TestTSIGRuntimeInitialPreparedRequired(t *testing.T) {
	s := newServingRuntimeFixture(t)
	runtime, err := newTSIGRuntime(s.opts.Store, s.opts.AuthKeys, s.opts.Keyrings, s.opts.Keyrings.Capture().Keys())
	if err != nil {
		t.Fatal("prepare original shared source owners")
	}
	// Constructor-only resources intentionally have no current snapshot. Run
	// the original coordinator, observe its real source-derived installation,
	// and join its worker before the fixture closes the borrowed root owners.
	providerCtx, cancelProviders := context.WithCancel(t.Context())
	providerDone := make(chan struct{})
	var providerErr error
	go func() {
		defer close(providerDone)
		providerErr = s.providers.Run(providerCtx)
	}()
	t.Cleanup(func() {
		cancelProviders()
		select {
		case <-providerDone:
			if providerErr != nil && !errors.Is(providerErr, context.Canceled) {
				t.Error("original provider coordinator did not exit cleanly")
			}
		case <-time.After(4 * time.Second):
			t.Error("original provider coordinator did not join")
		}
	})
	resourceWait, cancelResourceWait := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelResourceWait()
	for {
		installed := false
		err := s.resources.WithCurrent(resourceWait, func(*providerresources.Pinned) error {
			installed = true
			return nil
		})
		if err == nil && installed {
			break
		}
		if err != nil && !errors.Is(err, providerresources.ErrClosed) && resourceWait.Err() == nil {
			t.Fatal("original provider resource observation failed before installation")
		}
		select {
		case <-providerDone:
			t.Fatal("original provider coordinator exited before resource installation")
		case <-resourceWait.Done():
			t.Fatal("original provider coordinator did not install its actual resource snapshot")
		case <-time.After(10 * time.Millisecond):
		}
	}
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal("read original complete startup image")
	}
	baseline, err := s.captureHost(t.Context(), image)
	if baseline.Release != nil {
		baseline.Release()
	}
	if err != nil {
		t.Fatalf("fixture did not establish the otherwise valid original host capture: provider_resources_uninstalled_or_closed=%t", errors.Is(err, providerresources.ErrClosed))
	}
	s.opts.TSIG = runtime
	if s.opts.Keyrings.Capture().Prepared() != nil || runtime.captureAcquisition() != nil {
		t.Fatal("baseline-only owner claimed an admitted acquisition source")
	}
	deps, err := s.captureHost(t.Context(), image)
	if deps.Release != nil {
		deps.Release()
	}
	if !errors.Is(err, serving.ErrDependency) {
		t.Fatal("complete host capture admitted the initial unprepared file baseline")
	}
	if runtime.prepare(t.Context()) != nil || runtime.captureAcquisition() == nil {
		t.Fatal("explicit complete source did not become acquirable")
	}
	prepared, err := s.captureHost(t.Context(), image)
	if prepared.Release != nil {
		defer prepared.Release()
	}
	if err != nil || prepared.Current == nil || !prepared.Current() {
		t.Fatal("complete Prepared source did not admit the original complete host image")
	}
	t.Log("observed original provider-resource installation and valid baseline; Prepared=nil denied capture/acquisition; complete prepare admitted the original image")
}

// Both an unavailable first source and a missing first replica keep listeners
// operational but deny complete serving. In neither case may the static file
// baseline accidentally authorize a signed query while Prepared is absent.
func TestRunServeSQLiteStoredTSIGPendingStartupAndCleanup(t *testing.T) {
	for _, mode := range []string{"conflicting first source", "uninitialized follower"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(license.EnvKey, "")
			t.Setenv(license.EnvKeyFile, "")
			cfg, _ := mountedTestConfig(t)
			cfg.Auth.IdentityProviders = nil
			cfg.Cache.MaxEntries = 0
			cfg.Blocking.Enabled = false
			files, roots := tsigMountedCertificate(t)
			tsigMountedTLSListeners(t, cfg, files)
			var followerAuthority *replication.Authority
			if mode == "uninitialized follower" {
				scheme, err := runtimekinds.NewScheme()
				if err != nil {
					t.Fatal("prepare follower private scheme")
				}
				peer, err := sqlite.Open(filepath.Join(cfg.Node.DataDir, sqlite.FileName), sqlite.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: cfg.Node.Name})
				if err != nil {
					t.Fatal("prepare uninitialized follower")
				}
				if peer.Enroll(t.Context(), replication.Term{ClusterID: "tsig-pending-cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "initial"}) != nil {
					_ = peer.Close()
					t.Fatal("enroll actual follower without an image")
				}
				authority := peer.Authority()
				followerAuthority = &authority
				if peer.Close() != nil {
					t.Fatal("close original follower handle")
				}
			} else {
				st, closeStore := tsigMountedStore(t, cfg)
				sealer, err := tsigkeys.NewSealer(bytes.Repeat([]byte{8}, 32), nil)
				if err != nil {
					t.Fatal("prepare conflicting source encryption")
				}
				material, err := tsigkeys.NewMaterial(tsigkeys.Owner{ClusterID: st.Info().ClusterID, UID: "conflicting-first-source", KeyName: "updates.", Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x71}, 32))
				if err != nil {
					t.Fatal("prepare conflicting first source")
				}
				tsigMountedPersist(t, st, sealer, material, "")
				for _, object := range []store.Object{
					&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "denied"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "denied.test.", Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
					&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "denied", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "denied"}, Endpoints: []public.Endpoint{{DNSName: "host.denied.test.", RecordType: "A", Targets: []string{"192.0.2.93"}}}}},
				} {
					if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						t.Fatal("seed source that must remain unpublished")
					}
				}
				closeStore()
			}
			stop := tsigMountedStart(t, cfg)
			tsigMountedAwaitPendingListeners(t, cfg)
			baseline, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updates.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{7}, 32)})
			if err != nil {
				t.Fatal("prepare original file-only client")
			}
			for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
				query := new(dns.Msg)
				query.SetQuestion("host.denied.test.", dns.TypeA)
				query.Id = 0
				query.SetTsig("updates.", dns.HmacSHA256, 30, time.Now().Unix())
				wire, _, err := dns.TsigGenerateWithProvider(query, dnsserver.TSIGProvider(baseline), "", false)
				if err != nil {
					t.Fatal("sign original pending-source request")
				}
				answer := new(dns.Msg)
				if answer.Unpack(tsigMountedExchange(t, cfg, roots, protocol, wire)) != nil || answer.Rcode != dns.RcodeServerFailure || answer.Id != 0 || answer.IsTsig() != nil || len(answer.Answer) != 0 {
					t.Fatalf("pending %s source falsely admitted the file-only signer", protocol)
				}
			}
			stop()
			tsigMountedAssertPortsReusable(t, cfg)
			st, closeStore := tsigMountedStore(t, cfg)
			if followerAuthority != nil {
				if !reflect.DeepEqual(*followerAuthority, st.Authority()) {
					t.Fatal("pending TSIG startup changed follower authority")
				}
				if _, err := st.ReadGeneration(t.Context()); !errors.Is(err, replication.ErrReadOnly) {
					t.Fatal("pending TSIG startup invented a replica image")
				}
				for _, kind := range []store.Kind{"Kudurru", "DNSZone", "LocalUser", "TSIGKey"} {
					list, err := st.List(t.Context(), kind, store.ListOptions{})
					if err != nil || len(list.Items) != 0 {
						t.Fatal("pending TSIG follower bootstrapped authoritative objects")
					}
				}
			}
			closeStore()
		})
	}
}

func TestRunServeSQLiteStoredTSIGBindFailureReleasesStartup(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	files, _ := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	cfg.BlockPage.Enabled = true
	cfg.Listeners.BlockPage.HTTP = offlineFreeAddress(t, "tcp")
	cfg.Listeners.BlockPage.HTTPS = ""
	occupied, err := net.ListenPacket("udp", cfg.Listeners.DoQ[0].Address)
	if err != nil {
		t.Fatal("reserve exact conflicting encrypted socket")
	}
	defer func() { _ = occupied.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := runServe(ctx, cfg, ""); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatal("startup did not return the original encrypted-listener bind refusal")
	}
	if occupied.Close() != nil {
		t.Fatal("release fixture-owned conflicting socket")
	}
	tsigMountedAssertPortsReusable(t, cfg)
	_, closeStore := tsigMountedStore(t, cfg)
	closeStore()
}

func tsigMountedTLSListeners(t *testing.T, cfg *config.Config, files config.TLSFiles) {
	t.Helper()
	cfg.Listeners.DoT = []config.TLSListener{{Address: offlineFreeAddress(t, "tcp"), TLS: files}}
	cfg.Listeners.DoH = []config.HTTPListener{{Address: offlineFreeAddress(t, "tcp"), Path: "/dns-query", TLS: files}}
	cfg.Listeners.DoQ = []config.TLSListener{{Address: offlineFreeAddress(t, "udp"), TLS: files}}
	cfg.Listeners.DoH3 = []config.HTTPListener{{Address: offlineFreeAddress(t, "udp"), Path: "/dns-query", TLS: files}}
}

// Diagnostics contain only presence/readiness booleans and HTTP status codes.
// They never expose health bodies, authentication bodies, keys or source rows.
type tsigMountedPendingObservation struct {
	HealthReceived, HealthBodyRead, HealthDecoded bool
	HealthStatus                                  int
	DNSRegistered, DNSReady                       bool
	ServingRegistered, ServingReady               bool
	AuthStatus                                    int
}

func tsigMountedAwaitPendingListeners(t *testing.T, cfg *config.Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second}
	// The status route retains the existing two-second CaptureWait. Include
	// that wait plus the original one-second network budget while keeping
	// EVERY read inside the same original ten-second fixture context.
	statusClient := &http.Client{Timeout: 3 * time.Second}
	var last tsigMountedPendingObservation
	var sawDNSReady, sawServingUnavailable, sawAuth503 bool
	for {
		var observed tsigMountedPendingObservation
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.Listeners.Ops.Address+"/healthz", nil)
		if err != nil {
			t.Fatal("prepare pending listener observation")
		}
		response, err := client.Do(request)
		if err == nil {
			observed.HealthReceived, observed.HealthStatus = true, response.StatusCode
			var body struct{ Components map[string]health.Component }
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
			_ = response.Body.Close()
			observed.HealthBodyRead = readErr == nil
			observed.HealthDecoded = observed.HealthBodyRead && json.Unmarshal(data, &body) == nil
			if observed.HealthDecoded {
				dnsComponent, dnsRegistered := body.Components["dns"]
				servingComponent, servingRegistered := body.Components["serving"]
				observed.DNSRegistered, observed.DNSReady = dnsRegistered, dnsComponent.Ready
				observed.ServingRegistered, observed.ServingReady = servingRegistered, servingComponent.Ready
			}
		}
		// Observe independently rather than hiding this status behind the
		// health predicates. Preserve the original context and exact 503.
		observed.AuthStatus = tsigMountedPendingStatus(ctx, statusClient, cfg)
		last = observed
		sawDNSReady = sawDNSReady || (observed.HealthDecoded && observed.DNSReady)
		sawServingUnavailable = sawServingUnavailable || (observed.HealthDecoded && observed.ServingRegistered && !observed.ServingReady)
		sawAuth503 = sawAuth503 || observed.AuthStatus == http.StatusServiceUnavailable
		if observed.HealthDecoded && observed.DNSReady && !observed.ServingReady && observed.AuthStatus == http.StatusServiceUnavailable {
			t.Logf("pending source within original ten-second context: %+v", observed)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("pending TSIG source did not expose unavailable serving with operational listeners: last=%+v ever(dns_ready=%t serving_unavailable=%t auth_503=%t)", last, sawDNSReady, sawServingUnavailable, sawAuth503)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func tsigMountedPendingStatus(ctx context.Context, client *http.Client, cfg *config.Config) int {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.Listeners.API.Address+"/api/v1/auth/status", nil)
	if err != nil {
		return 0
	}
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

func tsigMountedAssertPortsReusable(t *testing.T, cfg *config.Config) {
	t.Helper()
	addresses := []struct{ network, address string }{{"tcp", cfg.Listeners.DNS[0].Address}, {"udp", cfg.Listeners.DNS[0].Address}, {"tcp", cfg.Listeners.API.Address}, {"tcp", cfg.Listeners.Ops.Address}}
	for _, listener := range cfg.Listeners.DoT {
		addresses = append(addresses, struct{ network, address string }{"tcp", listener.Address})
	}
	for _, listener := range cfg.Listeners.DoH {
		addresses = append(addresses, struct{ network, address string }{"tcp", listener.Address})
	}
	for _, listener := range cfg.Listeners.DoQ {
		addresses = append(addresses, struct{ network, address string }{"udp", listener.Address})
	}
	for _, listener := range cfg.Listeners.DoH3 {
		addresses = append(addresses, struct{ network, address string }{"udp", listener.Address})
	}
	if cfg.BlockPage.Enabled {
		addresses = append(addresses, struct{ network, address string }{"tcp", cfg.Listeners.BlockPage.HTTP})
	}
	for _, address := range addresses {
		if address.network == "udp" {
			listener, err := net.ListenPacket(address.network, address.address)
			if err != nil {
				t.Fatal("joined TSIG startup retained an original UDP socket")
			}
			if listener.Close() != nil {
				t.Fatal("close revalidated UDP socket")
			}
		} else {
			listener, err := net.Listen(address.network, address.address)
			if err != nil {
				t.Fatal("joined TSIG startup retained an original TCP socket")
			}
			if listener.Close() != nil {
				t.Fatal("close revalidated TCP socket")
			}
		}
	}
}
