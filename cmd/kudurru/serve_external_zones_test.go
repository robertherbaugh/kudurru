package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	k8ssource "github.com/robertherbaugh/kudurru/internal/sources/k8s"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
)

// Two mounted replicas serve a DNSZone of type external (D402) from an
// Ingress, a Gateway API HTTPRoute (through its accepting Gateway) and a
// LoadBalancer Service, over UDP and TCP, with no license key. Records follow
// their sources, a foreign record set holding a derived name is neither
// taken over nor served, record sets written outside the adapter are not
// served, and partitioning the elected writer moves the writes to the other
// replica without duplicates. The incoming leader diagnoses denied source
// reads while both replicas keep serving. Restoring permissions recovers the
// watches, and deleting the sources withdraws every name.
// An RFC 2136 UPDATE into the zone is refused with NOTAUTH and writes
// nothing, and 40 Ingresses created at once converge within 15 s.
func TestRunServeExternalZonesFromKubernetesSources(t *testing.T) {
	useExistingCluster := false
	control := &envtest.Environment{
		UseExistingCluster:    &useExistingCluster,
		BinaryAssetsDirectory: mountedKubeTestAssets(t),
		CRDDirectoryPaths:     []string{"../../deploy/kustomize/crd/bases", "../../internal/sources/k8s/testdata"},
		ErrorIfCRDPathMissing: true,
	}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	restConfig, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, networkingv1.AddToScheme, rbacv1.AddToScheme, public.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var identity corev1.Namespace
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &identity); err != nil {
		t.Fatal(err)
	}
	replicaCredentials, setSourceReads := externalZoneReplicaCredentials(t, control, admin)
	replica := func(name string, gate *partitionGate) *config.Config {
		cfg := offlineServeConfig(t, gate.server.URL)
		cfg.Node.Name = name
		cfg.Store.Kube.ExpectedClusterID = string(identity.UID)
		cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
		cfg.Listeners.DNS = []config.DNSListener{{Address: offlineFreeAddress(t, "tcp"), Protocols: []string{"udp", "tcp"}}}
		cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
		cfg.ExternalZones.Enabled = true
		cfg.Blocking.Enabled = false
		cfg.DNSSEC.Enabled = false
		raw, err := json.Marshal(map[string]any{"store": map[string]any{"driver": "kube"}, "externalZones": map[string]any{"enabled": true}, "blocking": map[string]any{"enabled": false}, "dnssec": map[string]any{"enabled": false}})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Source = filepath.Join(t.TempDir(), "kudurru.json")
		if err := os.WriteFile(cfg.Source, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	// The zone and the sources exist before either replica starts.
	if err := admin.Create(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "ext"}, Spec: public.DNSZoneSpec{
		ZoneName: "ext.example.", Type: public.ZoneTypeExternal, Nameservers: []string{"ns.ext.example."},
		External: &public.ExternalSpec{Ingress: true, Gateway: true, LoadBalancerServices: true},
	}}); err != nil {
		t.Fatal(err)
	}
	pathType := networkingv1.PathTypePrefix
	ingress := func(name, host string) *networkingv1.Ingress {
		return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pathType,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}}}}}}}}}}
	}
	setIngressAddress := func(in *networkingv1.Ingress, ip string) {
		t.Helper()
		in.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: ip}}
		if err := admin.Status().Update(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	web := ingress("web", "web.ext.example")
	if err := admin.Create(ctx, web); err != nil {
		t.Fatal(err)
	}
	setIngressAddress(web, "192.0.2.40")
	// A record set that already holds the name the adapter derives for the
	// "taken" Ingress belongs to someone else: it is never taken over.
	taken := ingress("taken", "taken.ext.example")
	if err := admin.Create(ctx, taken); err != nil {
		t.Fatal(err)
	}
	setIngressAddress(taken, "192.0.2.45")
	foreignName := k8ssource.RecordSetName("ingress", string(taken.UID), "ext")
	foreign := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: foreignName, Namespace: "default"}, Spec: public.DNSRecordSetSpec{
		ZoneRef: public.ObjectRef{Name: "ext"}, Endpoints: []public.Endpoint{{DNSName: "taken.ext.example", RecordType: "A", Targets: []string{"203.0.113.9"}}}}}
	if err := admin.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	// A record set written outside the adapter (kubectl, the API) that names
	// the external zone is not served, even for a name a source publishes.
	manual := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: "default"}, Spec: public.DNSRecordSetSpec{
		ZoneRef: public.ObjectRef{Name: "ext"}, Endpoints: []public.Endpoint{{DNSName: "web.ext.example", RecordType: "A", Targets: []string{"203.0.113.1"}}, {DNSName: "manual.ext.example", RecordType: "A", Targets: []string{"203.0.113.2"}}}}}
	if err := admin.Create(ctx, manual); err != nil {
		t.Fatal(err)
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	gateways := dyn.Resource(schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}).Namespace("default")
	routes := dyn.Resource(schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}).Namespace("default")
	gw, err := gateways.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "metadata": map[string]any{"name": "gw", "namespace": "default"},
		"spec": map[string]any{"gatewayClassName": "test", "listeners": []any{map[string]any{"name": "http", "port": int64(80), "protocol": "HTTP"}}},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gw.Object["status"] = map[string]any{"addresses": []any{map[string]any{"type": "IPAddress", "value": "192.0.2.50"}}}
	if _, err := gateways.UpdateStatus(ctx, gw, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	route, err := routes.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": map[string]any{"name": "shop", "namespace": "default"},
		"spec": map[string]any{"hostnames": []any{"shop.ext.example"}, "parentRefs": []any{map[string]any{"name": "gw"}}},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	route.Object["status"] = map[string]any{"parents": []any{map[string]any{"parentRef": map[string]any{"name": "gw"}, "controllerName": "example.com/gateway",
		"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted", "message": "", "lastTransitionTime": "2026-11-10T00:00:00Z"}}}}}
	if _, err := routes.UpdateStatus(ctx, route, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lb", Namespace: "default", Annotations: map[string]string{
		"external-dns.alpha.kubernetes.io/hostname": "svc.ext.example", "external-dns.kubernetes.io/ttl": "45"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Port: 53, TargetPort: intstr.FromInt32(53), Protocol: corev1.ProtocolUDP}}}}
	if err := admin.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	setServiceAddress := func(ip string) {
		t.Helper()
		var cur corev1.Service
		if err := admin.Get(ctx, client.ObjectKeyFromObject(svc), &cur); err != nil {
			t.Fatal(err)
		}
		cur.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
		if err := admin.Status().Update(ctx, &cur); err != nil {
			t.Fatal(err)
		}
	}
	setServiceAddress("192.0.2.60")

	// answer is the A target of name from cfg over network, "" for an
	// authoritative NXDOMAIN, and "?" for anything else.
	answer := func(cfg *config.Config, network, name string) (string, uint32) {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		r, _, err := (&dns.Client{Net: network, Timeout: time.Second}).ExchangeContext(ctx, q, cfg.Listeners.DNS[0].Address)
		if err != nil || !r.Authoritative {
			return "?", 0
		}
		if r.Rcode == dns.RcodeNameError && len(r.Answer) == 0 {
			return "", 0
		}
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
			return "?", 0
		}
		a, ok := r.Answer[0].(*dns.A)
		if !ok {
			return "?", 0
		}
		return a.A.String(), a.Hdr.Ttl
	}
	serves := func(cfg *config.Config, want map[string]string) bool {
		for name, target := range want {
			for _, network := range []string{"udp", "tcp"} {
				if got, _ := answer(cfg, network, name); got != target {
					return false
				}
			}
		}
		return true
	}
	await := func(d time.Duration, what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	holder := func() string {
		var lease coordinationv1.Lease
		if err := admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kudurru-controller"}, &lease); err != nil || lease.Spec.HolderIdentity == nil {
			return ""
		}
		return *lease.Spec.HolderIdentity
	}
	// derived lists the record sets the adapter maintains, by source kind.
	derived := func() map[string][]public.DNSRecordSet {
		var records public.DNSRecordSetList
		if err := admin.List(ctx, &records, client.InNamespace("default")); err != nil {
			t.Fatal(err)
		}
		out := map[string][]public.DNSRecordSet{}
		for _, rs := range records.Items {
			if zonemodel.ExternalSourceOwned(&rs) {
				out[rs.Labels[zonemodel.ExternalSourceLabel]] = append(out[rs.Labels[zonemodel.ExternalSourceLabel]], rs)
			}
		}
		return out
	}
	assertUnownedRecordSetUntouched := func(original *public.DNSRecordSet) {
		t.Helper()
		var cur public.DNSRecordSet
		if err := admin.Get(ctx, client.ObjectKeyFromObject(original), &cur); err != nil {
			t.Fatal(err)
		}
		// The zone controller writes its status (NotSelected), which moves
		// the resourceVersion; the spec, metadata and ownership stay the
		// foreign writer's.
		if cur.UID != original.UID || !reflect.DeepEqual(cur.Spec, original.Spec) || cur.Generation != original.Generation || len(cur.Annotations) != 0 || len(cur.Labels) != 0 || len(cur.OwnerReferences) != 0 || zonemodel.ExternalSourceOwned(&cur) {
			t.Fatalf("unowned record set %s changed: UID %s -> %s, generation %d -> %d, annotations %v, labels %v, owners %v", original.Name, original.UID, cur.UID, original.Generation, cur.Generation, cur.Annotations, cur.Labels, cur.OwnerReferences)
		}
	}
	first := map[string]string{"web.ext.example.": "192.0.2.40", "shop.ext.example.": "192.0.2.50", "svc.ext.example.": "192.0.2.60",
		"taken.ext.example.": "", "manual.ext.example.": "", "absent.ext.example.": ""}

	conditions := func() (ready, degraded *metav1.Condition) {
		var extZone public.DNSZone
		if err := admin.Get(ctx, client.ObjectKey{Name: "ext"}, &extZone); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(extZone.Status.Conditions, public.ConditionReady), meta.FindStatusCondition(extZone.Status.Conditions, public.ConditionDegraded)
	}
	isServing := func(ready *metav1.Condition) bool {
		return ready != nil && ready.Status == metav1.ConditionTrue && ready.Reason == public.ReasonServing
	}
	isHealthy := func() bool {
		ready, degraded := conditions()
		return isServing(ready) && degraded != nil && degraded.Status == metav1.ConditionFalse
	}
	gateA := newPartitionGate(t, replicaCredentials)
	credentialsB := rest.CopyConfig(replicaCredentials)
	var ingressProbe externalZoneIngressProbe
	credentialsB.Wrap(func(next http.RoundTripper) http.RoundTripper {
		ingressProbe.RoundTripper = next
		return &ingressProbe
	})
	gateB := newPartitionGate(t, credentialsB)
	// offlineServeConfig sets the process-global KUBECONFIG, which runServe
	// reads at startup: build and start each replica before the next config.
	cfgA := replica("external-a", gateA)
	_ = startStaticMount(t, cfgA)
	await(30*time.Second, "replica A leads", func() bool { return holder() == "external-a" })
	await(30*time.Second, "A serves every source over UDP and TCP", func() bool { return serves(cfgA, first) })
	if _, ttl := answer(cfgA, "udp", "svc.ext.example."); ttl != 45 {
		t.Fatal("the ttl annotation was not applied:", ttl)
	}
	cfgB := replica("external-b", gateB)
	_ = startStaticMount(t, cfgB)
	await(30*time.Second, "B serves every source", func() bool { return serves(cfgB, first) })
	if got := derived(); len(got) != 3 || len(got["ingress"]) != 1 || len(got["httproute"]) != 1 || len(got["service"]) != 1 || holder() != "external-a" {
		t.Fatal("a second replica changed leadership or duplicated record sets", got, holder())
	}
	assertUnownedRecordSetUntouched(foreign)
	assertUnownedRecordSetUntouched(manual)
	await(30*time.Second, "the zone is serving but reports the unselected record sets", func() bool {
		ready, degraded := conditions()
		return isServing(ready) && degraded != nil && degraded.Status == metav1.ConditionTrue && degraded.Reason == public.ReasonNotSelected
	})
	// Sources stay read-only: no finalizer, annotation or status of ours.
	var seen networkingv1.Ingress
	if err := admin.Get(ctx, client.ObjectKeyFromObject(web), &seen); err != nil {
		t.Fatal(err)
	}
	if len(seen.Finalizers) != 0 || seen.ResourceVersion != web.ResourceVersion {
		t.Fatal("the adapter wrote its source", seen.Finalizers, seen.ResourceVersion, web.ResourceVersion)
	}

	// Records follow their sources without removing unowned record sets.
	if err := admin.Delete(ctx, taken); err != nil {
		t.Fatal(err)
	}
	setIngressAddress(web, "192.0.2.41")
	moved := map[string]string{"web.ext.example.": "192.0.2.41", "shop.ext.example.": "192.0.2.50", "svc.ext.example.": "192.0.2.60", "taken.ext.example.": "", "manual.ext.example.": ""}
	await(30*time.Second, "both replicas follow the changed Ingress", func() bool { return serves(cfgA, moved) && serves(cfgB, moved) })
	for _, records := range []*public.DNSRecordSet{foreign, manual} {
		assertUnownedRecordSetUntouched(records)
		if err := admin.Delete(ctx, records); err != nil {
			t.Fatal(err)
		}
	}
	await(30*time.Second, "the external zone is healthy after removing the unowned fixtures", isHealthy)

	stored := func() map[string]public.DNSRecordSetSpec {
		records := map[string]public.DNSRecordSetSpec{}
		for _, group := range derived() {
			for _, rs := range group {
				records[string(rs.UID)] = rs.Spec
			}
		}
		return records
	}
	lastGood := stored()
	setSourceReads(false)
	restricted, err := dynamic.NewForConfig(replicaCredentials)
	if err != nil {
		t.Fatal(err)
	}
	await(10*time.Second, "the API server denies restricted Ingress lists", func() bool {
		_, err := restricted.Resource(networkingv1.SchemeGroupVersion.WithResource("ingresses")).List(ctx, metav1.ListOptions{Limit: 1})
		return apierrors.IsForbidden(err)
	})
	// Partition the elected writer so B starts a new term with no already
	// authorized source watches. A keeps serving its last data.
	gateA.partition(true)
	setServiceAddress("192.0.2.61")
	await(60*time.Second, "B acquires the Lease after A's partition", func() bool { return holder() == "external-b" })
	var ready, degraded *metav1.Condition
	deniedKinds := []string{"Ingress", "Gateway", "HTTPRoute", "GRPCRoute", "TLSRoute", "Service"}
	await(30*time.Second, "the zone reports SourcesIncomplete naming every denied source list", func() bool {
		ready, degraded = conditions()
		if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != public.ReasonSourcesIncomplete {
			return false
		}
		return !slices.ContainsFunc(deniedKinds, func(kind string) bool {
			return !strings.Contains(degraded.Message, kind+": list denied")
		})
	})
	if !isServing(ready) {
		t.Fatalf("source RBAC failure changed readiness: %+v", ready)
	}
	for _, kind := range deniedKinds {
		await(15*time.Second, "B counts the forbidden "+kind+" list", func() bool {
			value, ok := mountedMetric(t, cfgB.Listeners.Metrics.Address, "kudurru_external_zone_source_list_total", `kind="`+strings.ToLower(kind)+`"`, `result="forbidden"`)
			return ok && value > 0
		})
	}
	if !serves(cfgA, moved) {
		t.Fatal("the partitioned replica stopped serving its last-good answers")
	}
	gateA.partition(false)
	if got := stored(); !reflect.DeepEqual(got, lastGood) {
		t.Fatalf("source denial removed or changed stored record sets: got %v, want %v", got, lastGood)
	}
	if !serves(cfgA, moved) || !serves(cfgB, moved) {
		t.Fatal("a replica stopped serving its last-good answers during the source denial")
	}

	setSourceReads(true)
	await(25*time.Second, "B reopens an authorized Ingress watch after the grant", ingressProbe.watched.Load)
	listed := ingressProbe.listed.Load()
	if listed == nil {
		t.Fatal("B opened an Ingress watch without a successful list")
	}
	resumed := ingress("resumed", "resumed.ext.example")
	if err := admin.Create(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	setIngressAddress(resumed, "192.0.2.42")
	const periodicResync = 10 * time.Second
	deadline := listed.Add(periodicResync)
	await(time.Until(deadline), "B serves a new Ingress before the next periodic resync", func() bool {
		got, _ := answer(cfgB, "udp", "resumed.ext.example.")
		return time.Now().Before(deadline) && got == "192.0.2.42"
	})
	await(30*time.Second, "restoring source permissions clears Degraded", isHealthy)
	moved["svc.ext.example."] = "192.0.2.61"
	moved["resumed.ext.example."] = "192.0.2.42"
	await(30*time.Second, "both replicas serve the recovered sources", func() bool { return serves(cfgA, moved) && serves(cfgB, moved) })
	if holder() != "external-b" {
		t.Fatal("the healed former leader reclaimed the Lease", holder())
	}
	got := derived()
	if len(got) != 3 || len(got["ingress"]) != 2 || len(got["httproute"]) != 1 || len(got["service"]) != 1 {
		t.Fatal("the term change duplicated derived record sets", got)
	}
	if eps := got["service"][0].Spec.Endpoints; len(eps) != 1 || !slices.Equal(eps[0].Targets, []string{"192.0.2.61"}) || eps[0].RecordTTL != 45 {
		t.Fatal("derived Service record set", eps)
	}
	// Deleting the sources withdraws their names on both replicas.
	for _, in := range []*networkingv1.Ingress{web, resumed} {
		if err := admin.Delete(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if err := routes.Delete(ctx, "shop", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}
	gone := map[string]string{"web.ext.example.": "", "resumed.ext.example.": "", "shop.ext.example.": "", "svc.ext.example.": ""}
	await(30*time.Second, "deletion withdraws every name on both replicas", func() bool { return serves(cfgA, gone) && serves(cfgB, gone) })
	if got := derived(); len(got) != 0 {
		t.Fatal("deleted sources left derived record sets", got)
	}
	// An RFC 2136 UPDATE into the external zone is refused on both replicas
	// (NOTAUTH: no primary zone has that apex) and writes nothing.
	allRecordSets := func() int {
		var records public.DNSRecordSetList
		if err := admin.List(ctx, &records); err != nil {
			t.Fatal(err)
		}
		return len(records.Items)
	}
	before := allRecordSets()
	for _, cfg := range []*config.Config{cfgA, cfgB} {
		upd := new(dns.Msg)
		upd.SetUpdate("ext.example.")
		rr, err := dns.NewRR("upd.ext.example. 60 IN A 192.0.2.99")
		if err != nil {
			t.Fatal(err)
		}
		upd.Insert([]dns.RR{rr})
		r, _, err := (&dns.Client{Net: "tcp", Timeout: 2 * time.Second}).ExchangeContext(ctx, upd, cfg.Listeners.DNS[0].Address)
		if err != nil || r.Rcode != dns.RcodeNotAuth {
			t.Fatalf("UPDATE into the external zone on %s: %v %v", cfg.Node.Name, r, err)
		}
	}
	time.Sleep(time.Second)
	if got, _ := answer(cfgA, "udp", "upd.ext.example."); got != "" {
		t.Fatal("a refused UPDATE is served on A:", got)
	}
	if got, _ := answer(cfgB, "tcp", "upd.ext.example."); got != "" {
		t.Fatal("a refused UPDATE is served on B:", got)
	}
	if after := allRecordSets(); after != before {
		t.Fatalf("a refused UPDATE changed the record sets: %d -> %d", before, after)
	}

	// Many sources created at once converge within a few passes: the zone
	// controller's status writes (a serial per written record set) do not
	// refuse the rest of a pass.
	const bulk = 40
	bulkWant := map[string]string{}
	bulkGone := map[string]string{}
	var bulkSources []*networkingv1.Ingress
	for i := range bulk {
		in := ingress(fmt.Sprintf("bulk-%d", i), fmt.Sprintf("bulk-%d.ext.example", i))
		if err := admin.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		setIngressAddress(in, fmt.Sprintf("192.0.2.%d", 100+i))
		bulkSources = append(bulkSources, in)
		bulkWant[fmt.Sprintf("bulk-%d.ext.example.", i)] = fmt.Sprintf("192.0.2.%d", 100+i)
		bulkGone[fmt.Sprintf("bulk-%d.ext.example.", i)] = ""
	}
	created := time.Now()
	await(15*time.Second, "40 new Ingresses served on both replicas", func() bool { return serves(cfgA, bulkWant) && serves(cfgB, bulkWant) })
	t.Logf("%d Ingresses converged on both replicas in %s", bulk, time.Since(created).Round(100*time.Millisecond))
	if got := derived(); len(got) != 1 || len(got["ingress"]) != bulk {
		t.Fatal("bulk sources derived record sets", len(got["ingress"]))
	}
	for _, in := range bulkSources {
		if err := admin.Delete(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	await(15*time.Second, "the bulk sources are withdrawn", func() bool { return serves(cfgA, bulkGone) && serves(cfgB, bulkGone) })
}

func externalZoneReplicaCredentials(t *testing.T, control *envtest.Environment, admin client.Client) (*rest.Config, func(bool)) {
	t.Helper()
	const name = "external-zone-replica"
	user, err := control.AddUser(envtest.User{Name: name, Groups: []string{"system:authenticated"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := []string{"get", "list", "watch"}
	storeRules := []rbacv1.PolicyRule{
		{APIGroups: []string{public.Group}, Resources: []string{"*"}, Verbs: []string{"*"}},
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: read},
		{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{coordinationv1.GroupName}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update"}},
	}
	sourceRules := []rbacv1.PolicyRule{
		{APIGroups: []string{networkingv1.GroupName}, Resources: []string{"ingresses"}, Verbs: read},
		{APIGroups: []string{"gateway.networking.k8s.io"}, Resources: []string{"gateways", "httproutes", "grpcroutes", "tlsroutes"}, Verbs: read},
		{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: read},
	}
	storeRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name + "-store"}, Rules: storeRules}
	sourceRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name + "-sources"}, Rules: sourceRules}
	for _, role := range []*rbacv1.ClusterRole{storeRole, sourceRole} {
		if err := admin.Create(t.Context(), role); err != nil {
			t.Fatal(err)
		}
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role.Name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: name}},
		}
		if err := admin.Create(t.Context(), binding); err != nil {
			t.Fatal(err)
		}
	}
	return user.Config(), func(granted bool) {
		t.Helper()
		sourceRole.Rules = nil
		if granted {
			sourceRole.Rules = sourceRules
		}
		if err := admin.Update(t.Context(), sourceRole); err != nil {
			t.Fatal(err)
		}
	}
}

// externalZoneIngressProbe observes B's first successful list and watch after
// grants return. The list finishes before the adapter starts its periodic
// wait, so an answer within one resync of the list requires a watch wake-up.
type externalZoneIngressProbe struct {
	http.RoundTripper
	listed  atomic.Pointer[time.Time]
	watched atomic.Bool
}

func (probe *externalZoneIngressProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := probe.RoundTripper.RoundTrip(req)
	if err == nil && res.StatusCode == http.StatusOK && req.Method == http.MethodGet && req.URL.Path == "/apis/networking.k8s.io/v1/ingresses" {
		if req.URL.Query().Get("watch") == "true" {
			probe.watched.Store(true)
		} else {
			now := time.Now()
			probe.listed.CompareAndSwap(nil, &now)
		}
	}
	return res, err
}
