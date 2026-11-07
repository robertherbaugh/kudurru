package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestExternalDNSWebhookCLIRefusals(t *testing.T) {
	for _, args := range [][]string{
		{"externaldns-webhook"},
		{"externaldns-webhook", "--listen", "0.0.0.0:8888", "--upstream", "https://server.invalid"},
		{"externaldns-webhook", "--upstream", "http://server.invalid"},
		{"externaldns-webhook", "--upstream", "https://server.invalid"},
		{"externaldns-webhook", "--token", "fixture", "--upstream", "http://127.0.0.1:8889"},
		{"--transport", "kubernetes", "externaldns-webhook"},
	} {
		root := newRootWith(newCLIOpts())
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		if root.ExecuteContext(t.Context()) == nil {
			t.Fatal("invalid sidecar invocation accepted")
		}
	}
}

func TestExternalDNSWebhookCLIActualKubernetes(t *testing.T) {
	assets := mountedKubeTestAssets(t)
	control := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "kustomize", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	installed, err := control.Start()
	if err != nil {
		t.Fatal("webhook envtest start failed")
	}
	t.Cleanup(func() {
		if control.Stop() != nil {
			t.Error("webhook envtest stop failed")
		}
	})
	scheme := runtime.NewScheme()
	if public.AddToScheme(scheme) != nil || corev1.AddToScheme(scheme) != nil {
		t.Fatal("webhook fixture scheme failed")
	}
	admin, err := client.New(installed, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	if admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "relay"}}) != nil {
		t.Fatal("webhook fixture namespace failed")
	}
	st, err := kube.New(installed, kube.Options{Namespace: "relay", Scheme: scheme, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "relay"}, Spec: public.DNSZoneSpec{ZoneName: "relay.test.", Type: public.ZoneTypePrimary}}
	if _, err := st.Apply(ctx, zone, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(t.TempDir(), "upstream-token")
	if os.WriteFile(secretPath, []byte("sidecar-fixture-token-value"), 0o600) != nil {
		t.Fatal("webhook fixture token failed")
	}
	provider, err := externaldns.New(externaldns.Options{Store: st, Validator: v, Namespace: "relay", TokenFile: secretPath, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(provider)
	defer upstream.Close()
	caPath := filepath.Join(t.TempDir(), "upstream-ca.pem")
	if os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0o600) != nil {
		t.Fatal("webhook fixture CA failed")
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	if reservation.Close() != nil {
		t.Fatal("webhook fixture port release failed")
	}
	commandCtx, stop := context.WithCancel(ctx)
	defer stop()
	root := newRootWith(newCLIOpts())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"externaldns-webhook", "--listen", address, "--upstream", upstream.URL, "--upstream-token-file", secretPath, "--upstream-ca-file", caPath})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(commandCtx) }()
	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + address
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get(base + "/")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal("actual webhook negotiation failed")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("webhook CLI did not start")
		}
		select {
		case err := <-done:
			t.Fatalf("webhook CLI ended before listening: error=%t", err != nil)
		case <-time.After(10 * time.Millisecond):
		}
	}
	change := `{"create":[{"dnsName":"host.relay.test","recordType":"A","targets":["192.0.2.61"]}]}`
	response, err := client.Post(base+"/records", externaldns.MediaType, strings.NewReader(change))
	if err != nil {
		t.Fatal("webhook CLI write transport failed")
	}
	_ = response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("webhook CLI write status=%d", response.StatusCode)
	}
	response, err = client.Get(base + "/records")
	if err != nil {
		t.Fatal(err)
	}
	var endpoints []externaldns.Endpoint
	err = json.NewDecoder(response.Body).Decode(&endpoints)
	_ = response.Body.Close()
	if err != nil || len(endpoints) != 1 || endpoints[0].DNSName != "host.relay.test" {
		t.Fatal("actual Kubernetes endpoint not visible through CLI")
	}
	objects, err := st.List(ctx, "DNSRecordSet", store.ListOptions{Namespace: "relay"})
	if err != nil || len(objects.Items) != 1 || store.ManagedBy(objects.Items[0]) != externaldns.Manager {
		t.Fatal("sidecar changed upstream object ownership")
	}
	deleteBody, _ := json.Marshal(externaldns.Changes{Delete: []*externaldns.Endpoint{&endpoints[0]}})
	response, err = client.Post(base+"/records", externaldns.MediaType, bytes.NewReader(deleteBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal("webhook deletion refused")
	}
	objects, err = st.List(ctx, "DNSRecordSet", store.ListOptions{Namespace: "relay"})
	if err != nil || len(objects.Items) != 0 {
		t.Fatal("webhook deletion not committed")
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("webhook CLI shutdown failed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook CLI did not stop")
	}
}
