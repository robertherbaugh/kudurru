package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

type configuredBackend struct {
	ManagedBackend
	cfg Config
}

func (b *configuredBackend) Configuration() ManagementState { return ManagementState{Config: b.cfg} }

type recordingBackend struct {
	*LeaseStore
	calls       []string
	servingErr  error
	checks      int
	afterUpdate error
}

func newRecordingBackend(t *testing.T, cfg Config) *recordingBackend {
	t.Helper()
	store, err := OpenLeases(filepath.Join(t.TempDir(), "backend.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &recordingBackend{LeaseStore: store}
}

func (b *recordingBackend) CheckServing(context.Context) error {
	b.checks++
	return b.servingErr
}

func (b *recordingBackend) Offer(ctx context.Context, c Client, ip netip.Addr) (Lease, error) {
	b.calls = append(b.calls, "offer")
	return b.LeaseStore.Offer(ctx, c, ip)
}

func (b *recordingBackend) Acknowledge(ctx context.Context, c Client, ip netip.Addr, mode RequestMode) (Lease, error) {
	b.calls = append(b.calls, "ack")
	return b.LeaseStore.Acknowledge(ctx, c, ip, mode)
}

func (b *recordingBackend) Release(ctx context.Context, c Client, ip netip.Addr) error {
	b.calls = append(b.calls, "release")
	return b.LeaseStore.Release(ctx, c, ip)
}

func (b *recordingBackend) Decline(ctx context.Context, c Client, ip netip.Addr) error {
	b.calls = append(b.calls, "decline")
	return b.LeaseStore.Decline(ctx, c, ip)
}

func (b *recordingBackend) UpdateOptions(ctx context.Context, revision uint64, options PoolOptions) error {
	b.calls = append(b.calls, "options")
	if err := b.LeaseStore.UpdateOptions(ctx, revision, options); err != nil {
		return err
	}
	return b.afterUpdate
}

func (b *recordingBackend) PutReservation(ctx context.Context, revision uint64, reservation Reservation) error {
	b.calls = append(b.calls, "put")
	return b.LeaseStore.PutReservation(ctx, revision, reservation)
}

func (b *recordingBackend) DeleteReservation(ctx context.Context, revision uint64, mac string) error {
	b.calls = append(b.calls, "delete")
	return b.LeaseStore.DeleteReservation(ctx, revision, mac)
}

func (b *recordingBackend) Import(ctx context.Context, format string, data []byte) (ImportResult, error) {
	b.calls = append(b.calls, "import")
	return b.LeaseStore.Import(ctx, format, data)
}

func TestServiceBackendValidatesTopologyBeforeReplacement(t *testing.T) {
	cfg := relayConfig()
	cfg.FailoverServers = []ScopeServer{{NodeID: "one", Address: cfg.ServerIP}}
	svc := serviceForTest(t, cfg)
	prior := &configuredBackend{ManagedBackend: svc.stores[cfg.Name], cfg: cfg}
	if err := svc.SetBackend(cfg.Name, prior); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"name", func(c *Config) { c.Name = "another" }},
		{"interface", func(c *Config) { c.Interface = "other0" }},
		{"server IP", func(c *Config) { c.ServerIP = netip.MustParseAddr("198.51.100.3") }},
		{"subnet", func(c *Config) { c.Subnet = netip.MustParsePrefix("192.0.2.0/25") }},
		{"ownership", func(c *Config) { c.Management = "api" }},
		{"relay mode", func(c *Config) { c.RelayOnly = false }},
		{"probe mode", func(c *Config) { c.ConflictDetection = "icmp" }},
		{"probe timeout", func(c *Config) { c.ProbeTimeout = 300 * time.Millisecond }},
		{"relay source", func(c *Config) { c.TrustedRelays[0].SourceIP = netip.MustParseAddr("198.51.100.5") }},
		{"relay gateway", func(c *Config) { c.TrustedRelays[0].GatewayIP = netip.MustParseAddr("192.0.2.253") }},
		{"relay port", func(c *Config) { c.TrustedRelays[0].SourcePort = 1067 }},
		{"relay link", func(c *Config) { c.TrustedRelays[0].LinkSelection = c.RangeStart }},
		{"relay server ID", func(c *Config) { c.TrustedRelays[0].ServerIdentifier = c.Router }},
		{"failover node", func(c *Config) { c.FailoverServers[0].NodeID = "another" }},
		{"failover address", func(c *Config) { c.FailoverServers[0].Address = netip.MustParseAddr("198.51.100.3") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := normalizedConfig(cfg)
			tc.edit(&changed)
			candidate := &configuredBackend{ManagedBackend: prior, cfg: changed}
			if err := svc.SetBackend(cfg.Name, candidate); err == nil || !strings.Contains(err.Error(), "topology differs") {
				t.Fatalf("changed topology accepted: %v", err)
			}
			if svc.backends[cfg.Name] != prior || svc.scopes[cfg.Name].leases != prior {
				t.Fatal("rejected installation replaced the active backend")
			}
		})
	}
	if err := svc.SetBackend("missing", prior); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if err := svc.SetBackend(cfg.Name, nil); err == nil {
		t.Fatal("nil backend accepted")
	}
}

func TestServiceBackendAcceptsDefaultsAndValidatesMutableOptions(t *testing.T) {
	cfg := protocolConfig() // Blank name/ownership and zero timeout normalize to service defaults.
	svc := serviceForTest(t, cfg)
	backend := &configuredBackend{ManagedBackend: svc.stores["default"], cfg: cfg}
	backend.cfg.DomainName, backend.cfg.LeaseDuration = "backend.example", 10*time.Minute
	if err := svc.SetBackend("default", backend); err != nil {
		t.Fatal("normalized defaults or different mutable options rejected", err)
	}
	invalid := &configuredBackend{ManagedBackend: backend, cfg: backend.cfg}
	invalid.cfg.RangeEnd = cfg.RangeStart.Prev()
	if err := svc.SetBackend("default", invalid); err == nil || !strings.Contains(err.Error(), "backend configuration") {
		t.Fatal("invalid backend pool accepted", err)
	}
	if svc.backends["default"] != backend || svc.scopes["default"].leases != backend {
		t.Fatal("invalid pool replaced active backend")
	}
}

func TestServiceBackendMustBeInstalledBeforeListen(t *testing.T) {
	for _, stage := range []string{"before Listen", "listening", "running", "closed", "failed Listen"} {
		t.Run(stage, func(t *testing.T) {
			cfg := protocolConfig()
			svc := serviceForTest(t, cfg)
			first := &configuredBackend{ManagedBackend: svc.stores["default"], cfg: cfg}
			if err := svc.SetBackend("default", first); err != nil {
				t.Fatal(err)
			}
			conn := newServerConn()
			svc.listeners[0].listen = func(Config) (net.PacketConn, error) { return conn, nil }
			switch stage {
			case "listening", "running":
				if err := svc.Listen(); err != nil {
					t.Fatal(err)
				}
				if stage == "running" {
					ctx, cancel := context.WithCancel(context.Background())
					result := make(chan error, 1)
					go func() { result <- svc.Run(ctx) }()
					t.Cleanup(func() {
						cancel()
						if err := waitServerResult(t, result); err != nil {
							t.Error(err)
						}
					})
					packet := clientPacket(dhcpv4.MessageTypeDiscover)
					conn.input <- serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
					select {
					case <-conn.writes:
					case <-time.After(3 * time.Second):
						t.Fatal("service did not start")
					}
				}
			case "closed":
				if err := svc.Close(); err != nil {
					t.Fatal(err)
				}
			case "failed Listen":
				injected := errors.New("bind failure")
				svc.listeners[0].listen = func(Config) (net.PacketConn, error) { return nil, injected }
				if err := svc.Listen(); !errors.Is(err, injected) {
					t.Fatal(err)
				}
			}
			replacement := &configuredBackend{ManagedBackend: first, cfg: cfg}
			err := svc.SetBackend("default", replacement)
			if stage == "before Listen" {
				if err != nil || svc.backends["default"] != replacement || svc.scopes["default"].leases != replacement {
					t.Fatal("pre-Listen replacement failed", err)
				}
			} else if err == nil || svc.backends["default"] != first || svc.scopes["default"].leases != first {
				t.Fatal("backend changed after listener lifetime started", err)
			}
		})
	}
}

func TestServiceBackendTypedMutationsAndCallbackFence(t *testing.T) {
	cfg := protocolConfig()
	cfg.Management = "api"
	svc := serviceForTest(t, cfg)
	backend := newRecordingBackend(t, cfg)
	if err := svc.SetBackend("default", backend); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	localBefore := svc.stores["default"].Configuration()
	options := optionsFor(cfg)
	options.DomainName, options.NTPServers = "backend.example", []netip.Addr{cfg.Router}
	if err := svc.UpdateOptions(ctx, "default", backend.Configuration().Revision, options); err != nil {
		t.Fatal(err)
	}
	if svc.scopes["default"].cfg.DomainName != options.DomainName {
		t.Fatal("typed options did not refresh committed backend state")
	}
	reservation := Reservation{MAC: "02:00:00:00:00:03", IP: cfg.RangeStart.Next().Next(), Hostname: "reserved"}
	if err := svc.PutReservation(ctx, "default", backend.Configuration().Revision, reservation); err != nil {
		t.Fatal(err)
	}
	if got := svc.scopes["default"].cfg.Reservations; len(got) != 1 || got[0] != reservation {
		t.Fatal("reservation was not committed by custom backend", got)
	}
	if err := svc.DeleteReservation(ctx, "default", backend.Configuration().Revision, reservation.MAC); err != nil {
		t.Fatal(err)
	}
	if len(svc.scopes["default"].cfg.Reservations) != 0 {
		t.Fatal("deleted reservation survived configuration refresh")
	}
	data := []byte(fmt.Sprintf("%d 02:00:00:00:00:02 192.0.2.11 imported *\n", time.Now().Add(time.Hour).Unix()))
	result, err := svc.Import(ctx, "default", "dnsmasq", data)
	if err != nil || result.Imported != 1 {
		t.Fatal("custom backend import result was lost", result, err)
	}
	leases, err := backend.List(ctx)
	if err != nil || len(leases) != 1 || leases[0].Address != cfg.RangeStart.Next() {
		t.Fatal("import did not use custom backend", leases, err)
	}
	called := false
	err = svc.MutateScope(ctx, "default", func(context.Context, *LeaseStore) error { called = true; return nil })
	if !errors.Is(err, ErrReplicatedMutation) || called {
		t.Fatal("callback bypassed typed backend", called, err)
	}
	injected := errors.New("error after commit")
	backend.afterUpdate = injected
	options.DomainName = "committed.example"
	if err := svc.UpdateOptions(ctx, "default", backend.Configuration().Revision, options); !errors.Is(err, injected) {
		t.Fatal("backend error was lost", err)
	}
	if svc.scopes["default"].cfg.DomainName != options.DomainName {
		t.Fatal("committed configuration was stale after backend error")
	}
	if !reflect.DeepEqual(backend.calls, []string{"options", "put", "delete", "import", "options"}) {
		t.Fatal("typed operations bypassed backend", backend.calls)
	}
	if !reflect.DeepEqual(localBefore, svc.stores["default"].Configuration()) {
		t.Fatal("typed mutations wrote directly to local store")
	}
	if leases, err := svc.stores["default"].List(ctx); err != nil || len(leases) != 0 {
		t.Fatal("import bypassed backend and wrote local store", leases, err)
	}
	// A remote commit bypasses the service's local mutation lock. Dispatch must
	// refresh from the backend before building even an allocation-free INFORM.
	options.DomainName = "replicated.example"
	if err := backend.LeaseStore.UpdateOptions(ctx, backend.Configuration().Revision, options); err != nil {
		t.Fatal(err)
	}
	packet := clientPacket(dhcpv4.MessageTypeInform)
	packet.ClientIPAddr = net.IP(cfg.RangeStart.AsSlice())
	wire, _, err := svc.dispatch(ctx, cfg.Interface, packet.ToBytes(), packetSource(packet))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := dhcpv4.FromBytes(wire)
	if err != nil || reply.DomainName() != options.DomainName {
		t.Fatal("dispatch used stale options after a remote commit", err, reply)
	}
	// Guard management entry points before invoking the backend.
	for _, name := range []string{"default", "missing"} {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := svc.UpdateOptions(canceled, name, 0, options); err == nil {
			t.Fatal("canceled or unknown-scope mutation accepted", name)
		}
	}
	if len(backend.calls) != 5 {
		t.Fatal("rejected management request reached backend")
	}
}

func TestServiceBackendServingGateCoversDispatch(t *testing.T) {
	for _, message := range []dhcpv4.MessageType{dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeInform, dhcpv4.MessageTypeRequest, dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline} {
		t.Run(message.String(), func(t *testing.T) {
			cfg := protocolConfig()
			svc := serviceForTest(t, cfg)
			backend := newRecordingBackend(t, cfg)
			denied := errors.New("follower or no quorum")
			backend.servingErr = denied
			if err := svc.SetBackend("default", backend); err != nil {
				t.Fatal(err)
			}
			packet := clientPacket(message)
			var wantType dhcpv4.MessageType
			var wantCalls []string
			switch message {
			case dhcpv4.MessageTypeDiscover:
				wantType, wantCalls = dhcpv4.MessageTypeOffer, []string{"offer"}
			case dhcpv4.MessageTypeInform:
				packet.ClientIPAddr = net.IP(cfg.RangeStart.AsSlice())
				wantType = dhcpv4.MessageTypeAck
			case dhcpv4.MessageTypeRequest, dhcpv4.MessageTypeDecline:
				packet.UpdateOption(dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
				packet.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IP(cfg.RangeStart.AsSlice())))
				if message == dhcpv4.MessageTypeRequest {
					wantType, wantCalls = dhcpv4.MessageTypeNak, []string{"ack"}
				} else {
					wantCalls = []string{"decline"}
				}
			case dhcpv4.MessageTypeRelease:
				packet.ClientIPAddr = net.IP(cfg.RangeStart.AsSlice())
				packet.UpdateOption(dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
				wantCalls = []string{"release"}
			}
			ctx := context.Background()
			wire, destination, err := svc.dispatch(ctx, cfg.Interface, packet.ToBytes(), packetSource(packet))
			if !errors.Is(err, denied) || wire != nil || destination != nil || backend.checks != 1 || len(backend.calls) != 0 {
				t.Fatal("non-serving backend replied or mutated", wire, destination, err, backend.calls)
			}
			backend.servingErr = nil
			wire, _, err = svc.dispatch(ctx, cfg.Interface, packet.ToBytes(), packetSource(packet))
			if err != nil || backend.checks != 2 || !reflect.DeepEqual(backend.calls, wantCalls) {
				t.Fatal("serving backend not used", err, backend.calls)
			}
			if wantType == 0 {
				if wire != nil {
					t.Fatal("release or decline generated a reply")
				}
			} else {
				reply, err := dhcpv4.FromBytes(wire)
				if err != nil || reply.MessageType() != wantType {
					t.Fatal("incorrect response after serving resumed", err, reply)
				}
			}
		})
	}
}
