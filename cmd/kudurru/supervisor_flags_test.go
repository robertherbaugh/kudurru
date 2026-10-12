package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

// TestChartSupervisorArgs parses the arguments charts/kudurru renders for
// the dnsdist and keepalived containers (templates/_pod.tpl
// kudurru.edge.container, templates/edge.yaml): an unknown flag would
// crash-loop the edge pods.
func TestChartSupervisorArgs(t *testing.T) {
	for name, args := range map[string][]string{
		"edge run (deployment)": {"edge", "run", "--backends-from=endpointslice:kudurru-system/kudurru-headless", "--backend-port=5353",
			"--proxy-protocol-secret-file=/etc/kudurru/secrets/proxy/proxy-protocol-secret", "--pool-resolver=chashed",
			"--pool-authoritative=leastOutstanding", "--api=127.0.0.1:8083", "--console=127.0.0.1:5199",
			"--api-key-out=/run/kudurru/dnsdist-api.key", "--readiness-address=0.0.0.0:8084", "--tls-dir=/certs",
			"--dot=0.0.0.0:853", "--doh=0.0.0.0:443", "--doq=0.0.0.0:853", "--doh3=0.0.0.0:443",
			"--authoritative-cache", "--dynblocks", "--ebpf"},
		"edge run (sidecar)": {"edge", "run", "--backends=kudurru-0=127.0.0.1:5353", "--pool-resolver=chashed"},
		"vip run": {"vip", "run", "--vip=dns=10.0.0.53/24@eth0#53", "--node-name=worker-1",
			"--peers-from=nodes:kudurru.io/vip=true", "--hmac-key-file=/etc/kudurru/secrets/vrrp/vrrp-hmac",
			"--track-dnsdist-api=http://127.0.0.1:8083", "--dnsdist-api-key-file=/run/kudurru/dnsdist-api.key", "--require-backends=1"},
	} {
		root := newRoot()
		cmd, rest, err := root.Find(args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := cmd.ParseFlags(rest); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEdgeFrontendFlags(t *testing.T) {
	dir := t.TempDir()
	writeTestCert(t, dir)
	out := run(t, "edge", "render", "--backends", "127.0.0.1:5353", "--tls-dir", dir, "--dot", "0.0.0.0:8853", "--doh", ":8443")
	for _, want := range []string{"listen_address: 0.0.0.0:53\n", "listen_address: 0.0.0.0:8853\n    protocol: DoT", "listen_address: 0.0.0.0:8443\n    protocol: DoH"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "protocol: DoQ") {
		t.Error("--dot/--doh enabled DoQ too")
	}
	for name, args := range map[string][]string{
		"foreign host": {"edge", "render", "--backends", "127.0.0.1:5353", "--tls-dir", dir, "--dot", "10.9.9.9:853"},
		"bad port":     {"edge", "render", "--backends", "127.0.0.1:5353", "--tls-dir", dir, "--doq", "0.0.0.0:http"},
		"no tls":       {"edge", "render", "--backends", "127.0.0.1:5353", "--dot", "0.0.0.0:853"},
	} {
		if _, err := runErr(args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVIPPeersFromNodes(t *testing.T) {
	sel := map[string]string{"kudurru.io/vip": "true"}
	node := func(name, addr string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: sel},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: addr}}}}
	}
	cs := fake.NewClientset(node("worker-1", "10.0.0.11"), node("worker-2", "10.0.0.12"), node("worker-3", "10.0.0.13"))
	saved := nodesClient
	nodesClient = func() (corev1client.NodesGetter, error) { return cs.CoreV1(), nil }
	t.Cleanup(func() { nodesClient = saved })

	out := run(t, "vip", "render", "--vip", "dns=10.0.0.53/24@eth0#53", "--node-name", "worker-2", "--unicast-src", "10.0.0.12",
		"--peers-from", "nodes:kudurru.io/vip=true")
	for _, want := range []string{"router_id worker-2", "priority 190", "unicast_peer {\n        10.0.0.11\n        10.0.0.13\n    }"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if _, err := runErr("vip", "render", "--vip", "dns=10.0.0.53/24@eth0#53", "--node-name", "worker-2", "--unicast-src", "10.0.0.12",
		"--peers-from", "nodes:kudurru.io/vip=true", "--peers", "10.0.0.11"); err == nil {
		t.Error("--peers-from with --peers accepted")
	}
	if _, err := runErr("vip", "render", "--vip", "dns=10.0.0.53/24@eth0#53", "--node-name", "elsewhere", "--unicast-src", "10.0.0.12",
		"--peers-from", "nodes:kudurru.io/vip=true"); err == nil {
		t.Error("a node outside the selector accepted")
	}
}

// writeTestCert writes a self-signed tls.crt/tls.key pair into dir.
func writeTestCert(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "edge-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{"tls.crt": {Type: "CERTIFICATE", Bytes: der}, "tls.key": {Type: "EC PRIVATE KEY", Bytes: kb}} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
