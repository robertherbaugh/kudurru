package keepalived

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testNode(name string, labels map[string]string, addrs ...string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	for i, a := range addrs {
		kind := corev1.NodeInternalIP
		if i > 0 && strings.HasPrefix(a, "ext:") {
			kind, a = corev1.NodeExternalIP, strings.TrimPrefix(a, "ext:")
		}
		n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: kind, Address: a})
	}
	return n
}

func TestParsePeersFrom(t *testing.T) {
	if sel, err := ParsePeersFrom("nodes:kudurru.io/vip=true"); err != nil || sel != "kudurru.io/vip=true" {
		t.Errorf("selector %q %v", sel, err)
	}
	if sel, err := ParsePeersFrom("nodes:"); err != nil || sel != "" {
		t.Errorf("empty selector %q %v", sel, err)
	}
	if _, err := ParsePeersFrom("endpointslice:a/b"); err == nil {
		t.Error("non-nodes source accepted")
	}
}

func TestNodePeersList(t *testing.T) {
	vip := map[string]string{"kudurru.io/vip": "true"}
	cs := fake.NewClientset(
		testNode("node-c", vip, "10.0.0.13", "fd00::13"),
		testNode("node-a", vip, "fd00::11", "10.0.0.11"),
		testNode("node-b", vip, "10.0.0.12"),
		testNode("node-d", nil, "10.0.0.14"),                  // not selected
		testNode("node-e", vip, "fd00::15"),                   // no IPv4 address
		testNode("node-f", vip, "fd00::16", "ext:192.0.2.16"), // ExternalIP fallback
	)
	np := &NodePeers{Client: cs.CoreV1(), Selector: "kudurru.io/vip=true", Self: "node-b"}
	set, err := np.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := NodeSet{Peers: []string{"10.0.0.11", "10.0.0.13", "192.0.2.16"}, Index: 1, Count: 5}
	if !reflect.DeepEqual(set, want) {
		t.Errorf("set %+v, want %+v", set, want)
	}
	np.IPv6 = true
	if set, _ := np.List(context.Background()); !reflect.DeepEqual(set.Peers, []string{"fd00::11", "fd00::13", "fd00::15", "fd00::16"}) {
		t.Errorf("IPv6 peers %v", set.Peers)
	}
	np.Self = "node-d"
	if _, err := np.List(context.Background()); err == nil {
		t.Error("an unselected self accepted")
	}
	// The label pins the priority index.
	pinned := testNode("node-a", map[string]string{"kudurru.io/vip": "true", PriorityIndexLabel: "3"}, "10.0.0.11")
	if _, err := cs.CoreV1().Nodes().Update(context.Background(), pinned, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	np.Self, np.IPv6 = "node-a", false
	if set, err := np.List(context.Background()); err != nil || set.Index != 3 {
		t.Errorf("pinned index: %+v %v", set, err)
	}
	pinned.Labels[PriorityIndexLabel] = "first"
	_, _ = cs.CoreV1().Nodes().Update(context.Background(), pinned, metav1.UpdateOptions{})
	if _, err := np.List(context.Background()); err == nil {
		t.Error("a bad priority label accepted")
	}
}

func TestNodePeersSync(t *testing.T) {
	cs := fake.NewClientset(testNode("a", nil, "10.0.0.11"), testNode("b", nil, "10.0.0.12"))
	np := &NodePeers{Client: cs.CoreV1(), Self: "a", Interval: 10 * time.Millisecond}
	path := filepath.Join(t.TempDir(), "peers")
	set, err := np.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePeersFile(path, set.Peers); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPeersFile(path); err != nil || !reflect.DeepEqual(got, []string{"10.0.0.12"}) {
		t.Fatalf("peers file %v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		np.Sync(ctx, path, set.Peers, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	if _, err := cs.CoreV1().Nodes().Create(context.Background(), testNode("c", nil, "10.0.0.13"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitPeers := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, _ := ReadPeersFile(path)
			if reflect.DeepEqual(got, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("peers file not updated: %v, want %v", got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitPeers("10.0.0.12", "10.0.0.13")
	// Losing every peer keeps the previous file. One node at a time: Sync
	// re-lists every 10 ms, so a tick between two deletes rightly writes
	// the one-peer set.
	_ = cs.CoreV1().Nodes().Delete(context.Background(), "b", metav1.DeleteOptions{})
	waitPeers("10.0.0.13")
	_ = cs.CoreV1().Nodes().Delete(context.Background(), "c", metav1.DeleteOptions{})
	time.Sleep(50 * time.Millisecond)
	if got, _ := ReadPeersFile(path); !reflect.DeepEqual(got, []string{"10.0.0.13"}) {
		t.Errorf("peers file changed after losing every peer: %v", got)
	}
	cancel()
	<-done
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
