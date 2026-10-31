package interop

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// BIND notifies one replica selected by the load balancer. That replica fans
// out only a hint over authenticated internal HTTPS; both receive their own
// data from BIND before the ordinary refresh timer is due.
func TestBINDNotifyFansOutToIndependentSecondaries(t *testing.T) {
	kudurrutest.RequireE2E(t)
	const origin = "notify-replicas.test."
	const key = "notify-key.test."
	secret := newSecret(t)
	ring := secondaryKeyring(t, key, "hmac-sha256", secret)
	dir := t.TempDir()
	port := kudurrutest.FreePort(t)
	writeZone := func(serial uint32) {
		writeFile(t, filepath.Join(dir, "primary.db"), fmt.Sprintf(`$ORIGIN %s
$TTL 300
@ 537 IN SOA ns1.%s hostmaster.%s %d 61 13 120 47
@ 721 IN NS ns1.%s
ns1 87 IN A 127.0.0.1
www 117 IN A 192.0.2.80
`, origin, origin, origin, serial, origin))
	}
	conf := func(targetPort string) string {
		notify := "notify no;"
		if targetPort != "" {
			notify = fmt.Sprintf(`notify explicit; notify-delay 0; also-notify { 127.0.0.1 port %s key %q; };`, targetPort, key)
		}
		return namedOptions(dir, port) + fmt.Sprintf(`
key %q { algorithm hmac-sha256; secret %q; };
zone %q { type primary; file "primary.db"; allow-query { key %q; }; allow-transfer { key %q; }; %s };
`, key, secret, origin, key, key, notify)
	}
	writeZone(10)
	primary := startNamed(t, namedBinary(t), dir, conf(""), port)
	config := memory.New()
	t.Cleanup(func() { _ = config.Close() })
	dz := secondaryObject(origin, key)
	dz.Name = "notify-copy"
	dz.UID = "notify-config"
	dz.Spec.Secondary.Primaries = []string{primary.addr}
	if _, err := config.Apply(t.Context(), dz, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if err := config.Get(t.Context(), store.Key{Kind: "DNSZone", Name: dz.Name}, dz); err != nil {
		t.Fatal(err)
	}
	clock := new(secondaryServingClock)
	clock.set(time.Now().UTC())
	root, _ := secondaryRootForwarder(t, origin)
	second := startSecondaryServingRuntime(t, config, filepath.Join(t.TempDir(), "local.db"), ring, clock, root)
	second.startManager()
	awaitSecondaryServingStatus(t, second.manager, dz, func(s secondary.Status) bool { return s.Serial == 10 })

	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{42}, 32), NodeName: "peer"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, Event: func(event internalapi.Event) bool { return second.manager.Trigger(event.Identity) }})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	fanout := internalapi.NewEventFanout(&cluster.Static{SelfName: "first", Members: []cluster.Peer{{Name: "first"}, {Name: "second", Address: ln.Addr().String()}}}, internalapi.NewClient(creds, internalapi.ClientOptions{}), nil, nil)
	fanoutDone := make(chan struct{})
	go func() { defer close(fanoutDone); _ = fanout.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-fanoutDone })
	var acceptedMu sync.Mutex
	var accepted int
	onAccepted := func(identity string) {
		acceptedMu.Lock()
		accepted++
		acceptedMu.Unlock()
		fanout.Enqueue(internalapi.Event{Type: "secondary-notify", Identity: identity})
	}
	first := startSecondaryServingRuntime(t, config, filepath.Join(t.TempDir(), "local.db"), ring, clock, root, onAccepted)
	first.startManager()
	awaitSecondaryServingStatus(t, first.manager, dz, func(s secondary.Status) bool { return s.Serial == 10 })
	clock.set(clock.now().Add(2 * time.Second))
	_, targetPort, _ := net.SplitHostPort(first.addrs["udp"])
	writeZone(11)
	writeFile(t, filepath.Join(dir, "named.conf"), conf(targetPort))
	if err := primary.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	for _, replica := range []*secondaryServingRuntime{first, second} {
		// BIND may delay NOTIFY while draining its startup notification queue.
		// The injected SOA clock remains before refresh throughout this wait.
		if !kudurrutest.Eventually(15*time.Second, 10*time.Millisecond, func() bool {
			s, ok := replica.manager.StatusFor(dz)
			return ok && s.Serial == 11
		}) {
			s, _ := replica.manager.StatusFor(dz)
			t.Fatalf("NOTIFY did not refresh replica: %+v", s)
		}
		status, _ := replica.manager.StatusFor(dz)
		if status.Primary != primary.addr {
			t.Fatal("replica did not transfer from its configured primary")
		}
		assertSecondaryAnswer(t, replica, origin, 11)
	}
	acceptedMu.Lock()
	count := accepted
	acceptedMu.Unlock()
	if count == 0 {
		t.Fatal("no BIND NOTIFY reached the pipeline")
	}

	// An unsigned request cannot trigger the keyed zone, on either wire transport.
	for _, proto := range []string{"udp", "tcp"} {
		message := new(dns.Msg).SetNotify(origin)
		response, _, err := (&dns.Client{Net: proto, Timeout: time.Second}).ExchangeContext(t.Context(), message, first.addrs[proto])
		if err != nil || response.Rcode != dns.RcodeRefused {
			t.Fatalf("unsigned %s NOTIFY accepted: %v %v", proto, response, err)
		}
	}
	t.Log("BIND signed NOTIFY -> UDP listener -> local refresh + authenticated peer hint -> two independent BIND transfers before refresh deadline")
}
