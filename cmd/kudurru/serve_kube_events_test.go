package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/controller/kubeevents"
)

func TestRunServeKubeControllerEventsDoNotRequeue(t *testing.T) {
	assets := mountedKubeTestAssets(t)
	control := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
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
	decodeEvent := serializer.NewCodecFactory(scheme).UniversalDeserializer()
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
	if proxy.Transport, err = rest.TransportFor(actual); err != nil {
		t.Fatal(err)
	}
	var eventReads atomic.Int64
	var firstCreate sync.Once
	entered, release := make(chan struct{}), make(chan struct{})
	gate := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/v1/") && strings.HasSuffix(request.URL.Path, "/events") {
			switch request.Method {
			case http.MethodGet:
				eventReads.Add(1)
			case http.MethodPost:
				body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
				_ = request.Body.Close()
				if err != nil {
					http.Error(writer, err.Error(), http.StatusBadRequest)
					return
				}
				request.Body = io.NopCloser(bytes.NewReader(body))
				var event corev1.Event
				if _, _, err := decodeEvent.Decode(body, nil, &event); err != nil {
					http.Error(writer, err.Error(), http.StatusBadRequest)
					return
				}
				if event.InvolvedObject.Name == "event-proof" && event.Reason == controller.EventSerialBumped {
					firstCreate.Do(func() { close(entered) })
					select {
					case <-release:
					case <-request.Context().Done():
						return
					}
				}
			}
		}
		proxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(gate.Close)
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Blocking.Enabled, cfg.DNSSEC.Enabled = false, false
	stop := startStaticMount(t, cfg)
	defer stop()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "event-proof"},
		Spec: public.DNSZoneSpec{ZoneName: "event-proof.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.event-proof.test."}}}
	if err := admin.Create(t.Context(), zone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("controller did not attempt to create a core/v1 Event")
	}
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(zone), zone); err != nil || zone.Status.Serial == 0 {
		t.Fatalf("zone serial not persisted while Event write was blocked: serial=%d err=%v", zone.Status.Serial, err)
	}
	query := new(dns.Msg)
	query.SetQuestion(zone.Spec.ZoneName, dns.TypeSOA)
	dnsClient := &dns.Client{Timeout: 250 * time.Millisecond}
	if err := wait.PollUntilContextTimeout(t.Context(), 20*time.Millisecond, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		answer, _, err := dnsClient.ExchangeContext(ctx, query, cfg.Listeners.DNS[0].Address)
		if err == nil && len(answer.Answer) == 1 {
			soa, ok := answer.Answer[0].(*dns.SOA)
			return ok && soa.Serial == zone.Status.Serial, nil
		}
		return false, nil
	}); err != nil {
		t.Fatalf("DNS did not serve persisted serial %d while the Event write was blocked: %v", zone.Status.Serial, err)
	}
	reconciles := func() float64 {
		t.Helper()
		var total float64
		for _, result := range []string{"success", "conflict", "error"} {
			count, _ := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_controller_reconciles_total", `kind="DNSZone"`, `result="`+result+`"`)
			total += count
		}
		return total
	}
	before := reconciles()
	if before < 1 {
		t.Fatal("controller has not finished reconciling with the Event write held")
	}
	stableSince := time.Now()
	if err := wait.PollUntilContextTimeout(t.Context(), 25*time.Millisecond, 2*time.Second, true, func(context.Context) (bool, error) {
		if count := reconciles(); count != before {
			before, stableSince = count, time.Now()
		}
		return time.Since(stableSince) >= 600*time.Millisecond, nil
	}); err != nil {
		t.Fatalf("DNSZone reconciles did not settle with the Event write held: %v", err)
	}
	unblock()
	var events corev1.EventList
	if err := wait.PollUntilContextTimeout(t.Context(), 20*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		err := admin.List(ctx, &events, client.InNamespace("default"), client.MatchingFieldsSelector{
			Selector: fields.OneTermEqualSelector("involvedObject.uid", string(zone.UID)),
		})
		return len(events.Items) != 0, err
	}); err != nil {
		t.Fatalf("no Event for DNSZone UID %s: %v", zone.UID, err)
	}
	if len(events.Items) != 1 || events.Items[0].Reason != controller.EventSerialBumped || events.Items[0].Type != controller.EventNormal ||
		events.Items[0].InvolvedObject.Kind != "DNSZone" || events.Items[0].InvolvedObject.Name != zone.Name ||
		events.Items[0].ReportingController != kubeevents.DefaultComponent {
		t.Fatalf("unexpected DNSZone Events: %+v", events.Items)
	}
	for until := time.Now().Add(500 * time.Millisecond); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
		if after := reconciles(); after != before {
			t.Fatalf("Event write requeued a DNSZone: reconciles %v -> %v", before, after)
		}
	}
	if reads := eventReads.Load(); reads != 0 {
		t.Errorf("serve read or watched core/v1 Events %d times", reads)
	}
	if drops, ok := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_controller_events_dropped_total", `reason="queue_full"`); !ok || drops != 0 {
		t.Errorf("mounted queue_full counter = %v, present %v; want 0", drops, ok)
	}
	binary := filepath.Join(assets, "kubectl")
	if _, err := os.Stat(binary); err == nil {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "--kubeconfig", os.Getenv("KUBECONFIG"), "describe", "dnszone", zone.Name) //nolint:gosec // G204: envtest's bundled binary and disposable config
		output, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(output), controller.EventSerialBumped) {
			t.Fatalf("kubectl describe did not show SerialBumped: %v\n%s", err, output)
		}
		t.Log("envtest kubectl describe showed SerialBumped on DNSZone event-proof")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
