package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Actual runServe API routes provision the only stored key. File/previous/current
// encryption owners are admitted at separate restarts; no direct TSIG Store
// writer, debug endpoint, secret argv or replayed mutation constructs success.
func TestRunServeSQLiteTSIGManagementResealAndCurrentOnlyRestart(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "management"}, Spec: public.DNSZoneSpec{ZoneName: "management.test.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "management"}, Endpoints: []public.Endpoint{{DNSName: "seed.management.test.", RecordType: "A", Targets: []string{"192.0.2.97"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed independent desired DNS authority")
		}
	}
	closeStore()
	secret := bytes.Repeat([]byte{0x6a}, 32)
	clientKeys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "managed.", Algorithm: dns.HmacSHA256, Secret: secret})
	if err != nil {
		t.Fatal("prepare fixed original wire client")
	}
	var receipt api.TSIGKeyCommandReceipt
	for phase := range 3 {
		switch phase {
		case 1:
			t.Setenv(keys.EnvKey, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
			t.Setenv(keys.EnvPreviousKey, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
		case 2:
			t.Setenv(keys.EnvPreviousKey, "")
		}
		stop := tsigMountedStart(t, cfg)
		awaitMountedDNS(t, cfg, "seed.management.test.", "192.0.2.97")
		awaitControlMountedSource(t, cfg)
		transport := &http.Transport{DisableKeepAlives: true}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal("prepare original cookie transport")
		}
		client := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		t.Cleanup(transport.CloseIdleConnections)
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		tsigMountAwaitRead(t, client, base+"/auth/status", nil)
		var login api.SessionInfo
		controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		// Login can advance source; only read the actual route before the next
		// independent command, never replay login or a refused key command.
		tsigMountAwaitRead(t, client, base+"/tsigkeys", nil)
		switch phase {
		case 0:
			controlMountedRequest(t, client, base, http.MethodPost, "/tsigkeys", login.CSRF, map[string]any{"keyName": "managed.", "algorithm": "hmac-sha256", "secret": base64.StdEncoding.EncodeToString(secret)}, http.StatusCreated, &receipt)
			if !receipt.Committed || !receipt.Published || receipt.Removed || receipt.Key.UID == "" || receipt.Key.ResourceVersion == "" {
				t.Fatal("actual create receipt absent")
			}
		case 1:
			var list api.TSIGKeyList
			tsigMountAwaitRead(t, client, base+"/tsigkeys", &list)
			stored := tsigMountMetadata(t, list, "managed.")
			if stored.UID != receipt.Key.UID || stored.ResourceVersion != receipt.Key.ResourceVersion {
				t.Fatal("previous-key restart changed durable bindings")
			}
			before := stored
			controlMountedRequest(t, client, base, http.MethodPost, "/tsigkeys/managed./reseal", login.CSRF, map[string]any{"uid": stored.UID, "resourceVersion": stored.ResourceVersion}, http.StatusOK, &receipt)
			if !receipt.Committed || !receipt.Published || receipt.Key.UID != before.UID || receipt.Key.ResourceVersion == before.ResourceVersion {
				t.Fatal("actual reseal omitted intended envelope commit")
			}
		case 2:
			var list api.TSIGKeyList
			tsigMountAwaitRead(t, client, base+"/tsigkeys", &list)
			stored := tsigMountMetadata(t, list, "managed.")
			if stored.UID != receipt.Key.UID || stored.ResourceVersion != receipt.Key.ResourceVersion {
				t.Fatal("current-only restart lost actual API-resealed row")
			}
		}
		tsigMountSignedManagementQuery(t, cfg, clientKeys, dns.RcodeSuccess)
		if phase == 2 {
			before := receipt.Key
			controlMountedRequest(t, client, base, http.MethodPost, "/tsigkeys/managed./revoke", login.CSRF, map[string]any{"uid": before.UID, "resourceVersion": before.ResourceVersion}, http.StatusOK, &receipt)
			if !receipt.Committed || !receipt.Published || !receipt.Removed || receipt.Key.ResourceVersion != before.ResourceVersion {
				t.Fatal("actual revoke receipt did not retain historical binding")
			}
			tsigMountSignedManagementQuery(t, cfg, clientKeys, dns.RcodeNotAuth)
			awaitMountedDNS(t, cfg, "seed.management.test.", "192.0.2.97")
		}
		transport.CloseIdleConnections()
		stop()
		tsigMountedAssertPortsReusable(t, cfg)
	}
}

func tsigMountMetadata(t *testing.T, list api.TSIGKeyList, name string) api.TSIGKeyMetadata {
	t.Helper()
	for _, key := range list.Items {
		if key.KeyName == name && key.Source == "store" {
			return key
		}
	}
	t.Fatal("actual published stored metadata missing")
	return api.TSIGKeyMetadata{}
}

// One finite read-only observation budget. Overflow never earns decoded-ready
// credit, and safe diagnostics contain status/booleans only, never response data.
func tsigMountAwaitRead(t *testing.T, client *http.Client, address string, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			t.Fatal("construct bounded TSIG prerequisite read")
		}
		response, err := client.Do(request)
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
			closeErr := response.Body.Close()
			if response.StatusCode == http.StatusOK && readErr == nil && closeErr == nil && len(raw) <= 1<<20 {
				if out == nil {
					return
				}
				if json.Unmarshal(raw, out) == nil {
					return
				}
				t.Fatal("actual metadata response decode failed")
			}
			if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("TSIG prerequisite read status=%d", response.StatusCode)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("original TSIG prerequisite read deadline ended")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func tsigMountSignedManagementQuery(t *testing.T, cfg *config.Config, keys *dnsserver.StaticKeyring, rcode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	question := new(dns.Msg)
	question.SetQuestion("seed.management.test.", dns.TypeA)
	question.SetTsig("managed.", dns.HmacSHA256, 30, time.Now().Unix())
	if rcode == dns.RcodeSuccess {
		answer, _, err := (&dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigProvider: dnsserver.TSIGProvider(keys)}).ExchangeContext(ctx, question, cfg.Listeners.DNS[0].Address)
		if err != nil || answer == nil || answer.Rcode != rcode || answer.IsTsig() == nil || len(answer.Answer) != 1 {
			t.Fatal("actual managed key failed original signed response verification")
		}
		return
	}
	// BADKEY responses intentionally have an empty MAC. Read original wire
	// without asking the verifier to treat that protocol refusal as a success.
	wire, _, err := dns.TsigGenerateWithProvider(question, dnsserver.TSIGProvider(keys), "", false)
	if err != nil {
		t.Fatal("sign original revoked request")
	}
	answer := new(dns.Msg)
	if answer.Unpack(tsigMountedExchange(t, cfg, nil, "tcp", wire)) != nil || answer.Rcode != rcode || answer.IsTsig() == nil || answer.IsTsig().Error != dns.RcodeBadKey || answer.IsTsig().MAC != "" {
		t.Fatal("revoked managed key was accepted or refusal fabricated success")
	}
}

// A positively initialized replica is distinct from the earlier missing-image
// 503 fixture. Only the supported enrollment/validated install lifecycle moves
// the actual primary image; the only key provisioning uses the real API route.
func TestRunServeSQLiteTSIGManagementInitializedFollowerReadOnly(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "management"}, Spec: public.DNSZoneSpec{ZoneName: "management.test.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "management"}, Endpoints: []public.Endpoint{{DNSName: "seed.management.test.", RecordType: "A", Targets: []string{"192.0.2.97"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed independent primary DNS authority")
		}
	}
	closeStore()
	stopPrimary := tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "seed.management.test.", "192.0.2.97")
	awaitControlMountedSource(t, cfg)
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal("prepare original primary cookie transport")
	}
	client := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	tsigMountAwaitRead(t, client, base+"/auth/status", nil)
	var login api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	tsigMountAwaitRead(t, client, base+"/tsigkeys", nil)
	var receipt api.TSIGKeyCommandReceipt
	controlMountedRequest(t, client, base, http.MethodPost, "/tsigkeys", login.CSRF, tsigCreateBody("managed.", 0x6b), http.StatusCreated, &receipt)
	if !receipt.Committed || !receipt.Published {
		t.Fatal("primary actual-route receipt missing")
	}
	awaitControlMountedSource(t, cfg)
	stopPrimary()
	// The running primary is joined before reopening its supported store for
	// an offline snapshot read. No second live writer or raw SQL is introduced.
	st, closeStore = tsigMountedStore(t, cfg)
	setup, cancelSetup := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelSetup()
	followerConfig := *cfg
	followerConfig.Node = cfg.Node
	followerConfig.Node.Name = "tsig-initialized-follower"
	followerConfig.Node.DataDir = t.TempDir()
	followerConfig.Listeners = cfg.Listeners
	followerConfig.Listeners.DNS = []config.DNSListener{{Address: offlineFreeAddress(t, "tcp"), Protocols: []string{"udp", "tcp"}}}
	followerConfig.Listeners.API.Address = offlineFreeAddress(t, "tcp")
	followerConfig.Listeners.Ops.Address = offlineFreeAddress(t, "tcp")
	followerConfig.Listeners.Internal.Address = "127.0.0.1:0"
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal("prepare actual replica scheme")
	}
	follower, err := sqlite.Open(filepath.Join(followerConfig.Node.DataDir, sqlite.FileName), sqlite.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: followerConfig.Node.Name, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal("open actual initial follower")
	}
	t.Cleanup(func() { _ = follower.Close() })
	if follower.Enroll(setup, st.Authority().Term) != nil {
		t.Fatal("enroll actual follower authority")
	}
	if _, err := follower.BindAnchorLifecycle(setup); err != nil {
		t.Fatal("bind actual follower anchor lifecycle")
	}
	setup, err = follower.CaptureAnchorInstall(setup)
	if err != nil {
		t.Fatal("capture original follower before source image")
	}
	image, err := st.ReadGeneration(setup)
	if err != nil {
		t.Fatal("read joined primary complete image")
	}
	closeStore()
	validated, err := follower.ValidateImage(setup, *image)
	if err != nil {
		t.Fatal("validate initialized private image")
	}
	installed, err := follower.InstallReplica(setup, validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil || follower.Close() != nil {
		t.Fatal("install and close actual replica")
	}
	stopFollower := tsigMountedStart(t, &followerConfig)
	awaitMountedDNS(t, &followerConfig, "seed.management.test.", "192.0.2.97")
	followerBase := "http://" + followerConfig.Listeners.API.Address + api.BasePath
	var metadata api.TSIGKeyList
	tsigMountAwaitRead(t, client, followerBase+"/tsigkeys", &metadata)
	restored := tsigMountMetadata(t, metadata, "managed.")
	if restored.UID != receipt.Key.UID || restored.ResourceVersion != receipt.Key.ResourceVersion {
		t.Fatal("initialized follower changed durable key bindings")
	}
	clientKeys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "managed.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{0x6b}, 32)})
	if err != nil {
		t.Fatal("prepare original replicated credential client")
	}
	tsigMountSignedManagementQuery(t, &followerConfig, clientKeys, dns.RcodeSuccess)
	// Exactly one actual follower mutation attempt must refuse; no apparent
	// local-only success or refresh of the original primary's reviewed RV.
	controlMountedRequest(t, client, followerBase, http.MethodPost, "/tsigkeys/managed./replace", login.CSRF, tsigReplaceBody(restored, 0x6c), http.StatusServiceUnavailable, nil)
	stopFollower()
	check, closeCheck := tsigMountedStore(t, &followerConfig)
	after, err := check.ReadGeneration(t.Context())
	if err != nil || !after.Generation().Same(installed) || check.Authority().Writable() {
		t.Fatal("follower startup or refused management changed initialized image/authority")
	}
	closeCheck()
	tsigMountedAssertPortsReusable(t, &followerConfig)
}
