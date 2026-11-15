package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

const (
	zoneGroupAdmin    = "zg-admin"
	zoneGroupUser     = "grouper"
	zoneGroupPassword = "zone-group-user-password" // #nosec G101 -- the password of a test fixture account
	zoneGroupBinding  = "grouper-prod"
	zoneGroupZoner    = "zoner"
)

// zoneGroupSeed is what both drivers start with: zones a and b in group prod,
// zone c in no group, one record set in a and one in c, a zone-admin
// binding at zonegroup:prod for the local user grouper and one at zone:c for
// the local user zoner; zg-admin is the built-in administrator.
func zoneGroupSeed(t *testing.T) []store.Object {
	t.Helper()
	hash, err := users.NewHasher(0).Hash(t.Context(), zoneGroupPassword)
	if err != nil {
		t.Fatal(err)
	}
	set := func(name, zone string) *public.DNSRecordSet {
		return &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: zone}, Endpoints: []public.Endpoint{{DNSName: "www." + zone + ".test.", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.1"}}}}}
	}
	return []store.Object{
		// Seeding a local user ends first boot, so the administrator is seeded too.
		&internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: zoneGroupAdmin}, Spec: internalv1.LocalUserSpec{PasswordHash: hash, BuiltinAdmin: true, Groups: []string{}}},
		&internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: zoneGroupUser}, Spec: internalv1.LocalUserSpec{PasswordHash: hash, Groups: []string{}}},
		&internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: zoneGroupZoner}, Spec: internalv1.LocalUserSpec{PasswordHash: hash, Groups: []string{}}},
		zoneGroupZone("a", "prod"), zoneGroupZone("b", "prod"), zoneGroupZone("c", ""),
		set("in-a", "a"), set("in-c", "c"),
		&public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: zoneGroupBinding, Namespace: "default"}, Spec: public.RoleBindingSpec{Role: "zone-admin", Scope: public.BindingScope{Kind: "zonegroup", Name: "prod"}, Subjects: []public.BindingSubject{{Kind: "User", Name: zoneGroupUser}}}},
		&public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "zoner-c", Namespace: "default"}, Spec: public.RoleBindingSpec{Role: "zone-admin", Scope: public.BindingScope{Kind: "zone", Name: "c"}, Subjects: []public.BindingSubject{{Kind: "User", Name: zoneGroupZoner}}}},
	}
}

func zoneGroupZone(name, group string) *public.DNSZone {
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: name + ".test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + name + ".test."}}}
	if group != "" {
		z.Labels = map[string]string{public.ZoneGroupLabel: group}
	}
	return z
}

// zoneGroupZoneDoc is the whole-object PUT body of a seeded zone.
func zoneGroupZoneDoc(name, group string) map[string]any {
	meta := map[string]any{"name": name}
	if group != "" {
		meta["labels"] = map[string]any{public.ZoneGroupLabel: group}
	}
	return map[string]any{"apiVersion": public.GroupVersion.String(), "kind": "DNSZone", "metadata": meta,
		"spec": map[string]any{"zoneName": name + ".test.", "type": "primary", "nameservers": []any{"ns." + name + ".test."}}}
}

type zoneGroupClient struct {
	t    *testing.T
	c    *http.Client
	base string
	csrf string
}

func newZoneGroupClient(t *testing.T, base, username, password string) *zoneGroupClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	z := &zoneGroupClient{t: t, c: &http.Client{Jar: jar, Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: base}
	status, body := z.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: username, Password: password})
	var session api.SessionInfo
	if status != http.StatusOK || json.Unmarshal(body, &session) != nil || session.CSRF == "" {
		t.Fatalf("login %s: HTTP %d", username, status)
	}
	z.csrf = session.CSRF
	return z
}

// call sends one request. A 503 generation_pending is a refusal before any
// effect (the newest committed generation is not yet published here); it is
// asked again until it is decided.
func (z *zoneGroupClient) call(method, path string, body any) (int, []byte) {
	z.t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			z.t.Fatal(err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(z.t.Context(), method, z.base+path, bytes.NewReader(encoded))
		if err != nil {
			z.t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if z.csrf != "" {
			req.Header.Set("X-Kudurru-CSRF", z.csrf)
		}
		response, err := z.c.Do(req)
		if err != nil {
			z.t.Fatal("zone group request transport failed", method, path)
		}
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		pending := response.StatusCode == http.StatusServiceUnavailable && bytes.Contains(raw, []byte("generation_pending"))
		// The SQLite command path reports the same pending generation as
		// "current writable serving generation unavailable". Only requests
		// that are safe to repeat whatever happened (reads, whole-object
		// PUTs, sign-in) are asked again for it.
		if response.StatusCode == http.StatusServiceUnavailable && bytes.Contains(raw, []byte("current writable serving generation unavailable")) &&
			(method != http.MethodPost || path == "/auth/login") {
			pending = true
		}
		if !pending || time.Now().After(deadline) {
			return response.StatusCode, raw
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// await reads path until it answers want: the published state converges
// after a committed change.
func (z *zoneGroupClient) await(path string, want int) []byte {
	z.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, body := z.call(http.MethodGet, path, nil)
		if status == want {
			return body
		}
		if time.Now().After(deadline) {
			z.t.Fatalf("GET %s: HTTP %d, want %d: %s", path, status, want, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (z *zoneGroupClient) want(method, path string, body any, want int) []byte {
	z.t.Helper()
	status, raw := z.call(method, path, body)
	if status != want {
		z.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, status, want, raw)
	}
	return raw
}

// awaitZones reads GET /zones until it lists exactly names, each reported
// in group prod.
func (z *zoneGroupClient) awaitZones(names ...string) {
	z.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var list struct {
			Items []api.ZoneSummary `json:"items"`
		}
		status, body := z.call(http.MethodGet, "/zones", nil)
		var got []string
		if status == http.StatusOK && json.Unmarshal(body, &list) == nil {
			for _, item := range list.Items {
				if item.ZoneGroup == "prod" {
					got = append(got, item.Name)
				} else {
					got = append(got, item.Name+"(group "+item.ZoneGroup+")")
				}
			}
			slices.Sort(got)
			if slices.Equal(got, names) {
				return
			}
		}
		if time.Now().After(deadline) {
			z.t.Fatalf("GET /zones: HTTP %d lists %v, want %v", status, got, names)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// zoneGroupScenario is the D385 acceptance through the mounted router of one
// driver: the group-bound user reads and writes only member zones and their
// record sets, may edit a member zone that stays in the group but cannot
// move zones out of, between or into groups (nor can zoner, a zone-admin of
// zone c alone), loses a zone when the administrator removes its label,
// gains it back when the label returns, and loses everything when the
// binding is removed (its frozen session grant is revoked: revoked is the
// status the driver answers then).
func zoneGroupScenario(t *testing.T, grouper, zoner *zoneGroupClient, relabel func(zone, group string), unbind func(), revoked int) {
	t.Helper()
	var me struct {
		Roles             []string                       `json:"roles"`
		ScopedPermissions map[string]map[string][]string `json:"scopedPermissions"`
	}
	if err := json.Unmarshal(grouper.want(http.MethodGet, "/auth/me", nil, http.StatusOK), &me); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(me.Roles, "zone-admin@zonegroup:prod") || !slices.Contains(me.ScopedPermissions["zonegroup:prod"]["records"], "write") || len(me.ScopedPermissions["zone:a"]) != 0 {
		t.Fatalf("/auth/me does not report the zone group grant: roles=%v scoped=%v", me.Roles, me.ScopedPermissions)
	}
	grouper.awaitZones("a", "b")
	grouper.await("/zones/a", http.StatusOK)
	grouper.await("/zones/c", http.StatusForbidden)
	grouper.await("/zones/b/records", http.StatusOK)
	grouper.await("/objects/dnsrecordsets/default/in-a", http.StatusOK)
	grouper.await("/objects/dnsrecordsets/default/in-c", http.StatusForbidden)
	var sets struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(grouper.want(http.MethodGet, "/objects/dnsrecordsets", nil, http.StatusOK), &sets); err != nil {
		t.Fatal(err)
	}
	for _, item := range sets.Items {
		if item["metadata"].(map[string]any)["name"] == "in-c" {
			t.Fatal("record set list shows a record set of a zone outside the group")
		}
	}
	record := public.Endpoint{DNSName: "api", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.10"}}
	grouper.want(http.MethodPost, "/zones/a/records", record, http.StatusCreated)
	grouper.want(http.MethodPost, "/zones/c/records", record, http.StatusForbidden)
	// Group membership is access control: only cluster scope changes it. A
	// member may write a zone that stays in the group, but not take it out
	// or move it to another group; the zone admin of c may write c, but not
	// put it into a group.
	grouper.want(http.MethodPut, "/objects/dnszones/a", zoneGroupZoneDoc("a", "prod"), http.StatusOK)
	grouper.want(http.MethodPut, "/objects/dnszones/a", zoneGroupZoneDoc("a", ""), http.StatusForbidden)
	grouper.want(http.MethodPut, "/objects/dnszones/a", zoneGroupZoneDoc("a", "dev"), http.StatusForbidden)
	grouper.want(http.MethodPut, "/objects/dnszones/c", zoneGroupZoneDoc("c", "prod"), http.StatusForbidden)
	zoner.want(http.MethodPut, "/objects/dnszones/c", zoneGroupZoneDoc("c", ""), http.StatusOK)
	zoner.want(http.MethodPut, "/objects/dnszones/c", zoneGroupZoneDoc("c", "prod"), http.StatusForbidden)
	grouper.awaitZones("a", "b")

	// The administrator takes b out of the group: the binding stops covering it.
	relabel("b", "")
	grouper.await("/zones/b", http.StatusForbidden)
	grouper.awaitZones("a")
	grouper.want(http.MethodPost, "/zones/b/records", record, http.StatusForbidden)
	grouper.await("/zones/b/records", http.StatusForbidden)
	// And puts it back.
	relabel("b", "prod")
	grouper.await("/zones/b", http.StatusOK)
	grouper.want(http.MethodPost, "/zones/b/records", record, http.StatusCreated)

	// Removing the binding takes the group away. The complete SQLite runtime
	// also ends the session whose grant it revoked (401); the Kubernetes
	// driver refuses the session's frozen grant (403).
	unbind()
	denied := func(status int) bool { return status == revoked }
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, body := grouper.call(http.MethodGet, "/zones/a", nil)
		if denied(status) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /zones/a after the binding was removed: HTTP %d: %s", status, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status, body := grouper.call(http.MethodPost, "/zones/a/records", public.Endpoint{DNSName: "late", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.11"}}); !denied(status) {
		t.Fatalf("record write after the binding was removed: HTTP %d: %s", status, body)
	}
}

func zoneGroupLicense(t *testing.T) string {
	t.Helper()
	sign := trustTestKey(t)
	return sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureRBAC}})
}

func TestRunServeSQLiteZoneGroupBinding(t *testing.T) {
	licenseKey := zoneGroupLicense(t)
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range zoneGroupSeed(t) {
		manager := store.ManagerAPI
		if _, private := object.(*internalv1.LocalUser); private {
			manager = store.ManagerSystem
		}
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: manager}); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	startLicensedMount(t, cfg, licenseKey)
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	admin := newZoneGroupClient(t, base, zoneGroupAdmin, zoneGroupPassword)
	grouper := newZoneGroupClient(t, base, zoneGroupUser, zoneGroupPassword)
	zoner := newZoneGroupClient(t, base, zoneGroupZoner, zoneGroupPassword)
	zoneGroupScenario(t, grouper, zoner, func(zone, group string) {
		admin.want(http.MethodPut, "/objects/dnszones/"+zone, zoneGroupZoneDoc(zone, group), http.StatusOK)
	}, func() {
		var binding public.RoleBinding
		if err := json.Unmarshal(admin.want(http.MethodGet, "/iam/rolebindings/default/"+zoneGroupBinding, nil, http.StatusOK), &binding); err != nil || binding.UID == "" {
			t.Fatal("binding read failed", err)
		}
		admin.want(http.MethodDelete, "/iam/rolebindings/default/"+zoneGroupBinding, api.RoleBindingReview{UID: string(binding.UID), ResourceVersion: binding.ResourceVersion}, http.StatusNoContent)
	}, http.StatusUnauthorized)
}

func TestRunServeKubeZoneGroupBinding(t *testing.T) {
	licenseKey := zoneGroupLicense(t)
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
	kubeAdmin, err := client.New(actual, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var namespace corev1.Namespace
	if err := kubeAdmin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &namespace); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(actual.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if proxy.Transport, err = rest.TransportFor(actual); err != nil {
		t.Fatal(err)
	}
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.Auth.Session.Secure = "never"

	// The local user is private state in the kudurru store; the public
	// objects are ordinary Kubernetes objects.
	st, err := kube.New(actual, kube.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), ExpectedClusterID: string(namespace.UID), StartupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, object := range zoneGroupSeed(t) {
		if _, private := object.(*internalv1.LocalUser); private {
			if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := kubeAdmin.Create(t.Context(), object.(client.Object)); err != nil {
			t.Fatal(err)
		}
	}
	startLicensedMount(t, cfg, licenseKey)
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	admin := newZoneGroupClient(t, base, zoneGroupAdmin, zoneGroupPassword)
	grouper := newZoneGroupClient(t, base, zoneGroupUser, zoneGroupPassword)
	zoner := newZoneGroupClient(t, base, zoneGroupZoner, zoneGroupPassword)
	// The seeded objects belong to the kubectl client's field manager. The
	// administrator removes b's label through the API: the PUT must take the
	// label away although kubectl owns it too (server-side apply alone would
	// keep it and answer 200). An operator puts it back with kubectl, which
	// kudurru must honor as well.
	zoneGroupScenario(t, grouper, zoner, func(zone, group string) {
		if group == "" {
			var written public.DNSZone
			if err := json.Unmarshal(admin.want(http.MethodPut, "/objects/dnszones/"+zone, zoneGroupZoneDoc(zone, ""), http.StatusOK), &written); err != nil {
				t.Fatal(err)
			}
			if g := public.ZoneGroupOf(&written); g != "" {
				t.Fatalf("the API PUT answered zone %s in group %q", zone, g)
			}
			var stored public.DNSZone
			if err := kubeAdmin.Get(t.Context(), client.ObjectKey{Name: zone}, &stored); err != nil {
				t.Fatal(err)
			}
			if g := public.ZoneGroupOf(&stored); g != "" {
				t.Fatalf("the apiserver keeps zone %s in group %q after the API removed the label", zone, g)
			}
			return
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		var z public.DNSZone
		if err := kubeAdmin.Get(ctx, client.ObjectKey{Name: zone}, &z); err != nil {
			t.Fatal(err)
		}
		if group == "" {
			delete(z.Labels, public.ZoneGroupLabel)
		} else {
			if z.Labels == nil {
				z.Labels = map[string]string{}
			}
			z.Labels[public.ZoneGroupLabel] = group
		}
		if err := kubeAdmin.Update(ctx, &z); err != nil {
			t.Fatal(err)
		}
	}, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := kubeAdmin.Delete(ctx, &public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: zoneGroupBinding, Namespace: "default"}}); err != nil {
			t.Fatal(err)
		}
	}, http.StatusForbidden)
}
