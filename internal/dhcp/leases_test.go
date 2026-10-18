package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func leaseTestClient(n int) Client {
	return Client{MAC: fmt.Sprintf("02:00:00:00:%02x:%02x", n>>8, n&255), Hostname: fmt.Sprintf("client-%d", n)}
}

func leaseTestOpen(t *testing.T, cfg Config) (*LeaseStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leases.db")
	s, err := OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func leaseTestOffer(t *testing.T, s *LeaseStore, c Client, ip netip.Addr) Lease {
	t.Helper()
	l, err := s.Offer(context.Background(), c, ip)
	if err != nil {
		t.Fatal(err)
	}
	l.GrantedDuration = 0 // Helpers compare persisted state, not response metadata.
	return l
}

func leaseTestBind(t *testing.T, s *LeaseStore, c Client) Lease {
	t.Helper()
	offer := leaseTestOffer(t, s, c, netip.Addr{})
	l, err := s.Acknowledge(context.Background(), c, offer.Address, Selecting)
	if err != nil {
		t.Fatal(err)
	}
	l.GrantedDuration = 0 // Helpers compare persisted state, not response metadata.
	return l
}

func leaseTestList(t *testing.T, s *LeaseStore) []Lease {
	t.Helper()
	ls, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

func TestLeaseOfferBindingAndExpiry(t *testing.T) {
	cfg := leaseTestConfig()
	s, _ := leaseTestOpen(t, cfg)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	c := leaseTestClient(1)
	offer := leaseTestOffer(t, s, c, cfg.RangeEnd)
	if offer.Address != cfg.RangeEnd || offer.State != "offered" || !offer.ExpiresAt.Equal(now.Add(time.Minute)) || offer.ClientID != c.MAC {
		t.Fatalf("incorrect offer: %+v", offer)
	}
	for _, mode := range []RequestMode{InitReboot, Renewing} {
		if _, err := s.Acknowledge(context.Background(), c, offer.Address, mode); !errors.Is(err, ErrUnknownLease) {
			t.Fatalf("unbound request mode %v: %v", mode, err)
		}
	}
	bound, err := s.Acknowledge(context.Background(), c, offer.Address, Selecting)
	if err != nil || bound.State != "bound" || !bound.ExpiresAt.Equal(now.Add(cfg.LeaseDuration)) {
		t.Fatalf("incorrect binding: %+v, %v", bound, err)
	}
	now = now.Add(time.Hour)
	rediscover := leaseTestOffer(t, s, c, cfg.RangeStart)
	if leasePersistent(rediscover) != leasePersistent(bound) {
		t.Fatalf("DISCOVER changed live binding: %+v -> %+v", bound, rediscover)
	}
	renewed, err := s.Acknowledge(context.Background(), c, bound.Address, Renewing)
	if err != nil || !renewed.ExpiresAt.Equal(now.Add(cfg.LeaseDuration)) {
		t.Fatalf("renewal failed: %+v %v", renewed, err)
	}
	now = renewed.ExpiresAt
	reused := leaseTestOffer(t, s, leaseTestClient(2), renewed.Address)
	if reused.Address != renewed.Address || reused.MAC == renewed.MAC {
		t.Fatalf("expired binding was not reused: %+v", reused)
	}
	now = reused.ExpiresAt
	reused = leaseTestOffer(t, s, c, reused.Address)
	if reused.ClientID != c.MAC {
		t.Fatalf("expired offer not reused: %+v", reused)
	}
}

func TestLeaseUnknownRequestsStayUnknown(t *testing.T) {
	cfg := leaseTestConfig()
	s, _ := leaseTestOpen(t, cfg)
	known, unknown := leaseTestClient(1), leaseTestClient(2)
	bound := leaseTestBind(t, s, known)
	for _, mode := range []RequestMode{InitReboot, Renewing} {
		for _, address := range []netip.Addr{bound.Address, cfg.RangeEnd, netip.MustParseAddr("198.51.100.10"), cfg.ServerIP, {}} {
			if l, err := s.Acknowledge(context.Background(), unknown, address, mode); !errors.Is(err, ErrUnknownLease) || l.Address.IsValid() {
				t.Fatalf("unknown mode=%v address=%v: lease=%+v err=%v", mode, address, l, err)
			}
		}
		for _, address := range []netip.Addr{cfg.RangeEnd, netip.MustParseAddr("198.51.100.10"), cfg.ServerIP} {
			if _, err := s.Acknowledge(context.Background(), known, address, mode); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("known client requested wrong address %v: %v", address, err)
			}
		}
		if _, err := s.Acknowledge(context.Background(), known, bound.Address, mode); err != nil {
			t.Fatalf("known binding mode=%v: %v", mode, err)
		}
	}
	if ls := leaseTestList(t, s); len(ls) != 1 || ls[0].Address != bound.Address {
		t.Fatalf("unknown requests mutated leases: %+v", ls)
	}
	if _, err := s.Acknowledge(context.Background(), unknown, cfg.RangeEnd, Selecting); !errors.Is(err, ErrUnknownLease) {
		t.Fatalf("selecting without offer: %v", err)
	}
}

func TestLeaseIdentityAndOwnership(t *testing.T) {
	cfg := leaseTestConfig()
	cfg.RangeEnd = netip.MustParseAddr("192.0.2.20")
	s, _ := leaseTestOpen(t, cfg)
	ctx := context.Background()
	c := leaseTestClient(1)
	bound := leaseTestBind(t, s, c)
	// An opaque option 61 matching the fallback MAC text is a different identity.
	optionClient := c
	optionClient.ID = c.MAC
	second := leaseTestBind(t, s, optionClient)
	if second.Address == bound.Address {
		t.Fatal("option 61 collided with MAC fallback")
	}
	for _, intruder := range []Client{leaseTestClient(2), {MAC: c.MAC, ID: "another-id"}, {MAC: leaseTestClient(2).MAC, ID: optionClient.ID}} {
		for _, address := range []netip.Addr{bound.Address, second.Address} {
			if err := s.Release(ctx, intruder, address); !errors.Is(err, ErrUnknownLease) {
				t.Fatalf("foreign release accepted: %v", err)
			}
			if err := s.Decline(ctx, intruder, address); !errors.Is(err, ErrUnknownLease) {
				t.Fatalf("foreign decline accepted: %v", err)
			}
		}
	}
	if ls := leaseTestList(t, s); len(ls) != 2 {
		t.Fatalf("foreign requests changed leases: %+v", ls)
	}
	if err := s.Release(ctx, c, bound.Address); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, c, bound.Address); !errors.Is(err, ErrUnknownLease) {
		t.Fatalf("repeated release: %v", err)
	}
	if got := leaseTestOffer(t, s, leaseTestClient(3), bound.Address); got.Address != bound.Address {
		t.Fatalf("released address unavailable: %+v", got)
	}
}

func TestLeaseReservationAndInfrastructureProtection(t *testing.T) {
	cfg := leaseTestConfig()
	cfg.RangeStart, cfg.RangeEnd = cfg.ServerIP, netip.MustParseAddr("192.0.2.5")
	cfg.Router = netip.MustParseAddr("192.0.2.2")
	r := Reservation{MAC: "02-00-00-AA-00-01", IP: netip.MustParseAddr("192.0.2.3"), Hostname: "printer"}
	outside := Reservation{MAC: leaseTestClient(9).MAC, IP: netip.MustParseAddr("192.0.2.100")}
	cfg.Reservations = []Reservation{r, outside}
	s, _ := leaseTestOpen(t, cfg)
	// Mutating the caller's backing slice cannot change allocation policy.
	cfg.Reservations[0].IP = netip.MustParseAddr("192.0.2.4")
	first := leaseTestOffer(t, s, leaseTestClient(1), r.IP)
	second := leaseTestOffer(t, s, leaseTestClient(2), cfg.ServerIP)
	if first.Address.String() != "192.0.2.4" || second.Address.String() != "192.0.2.5" {
		t.Fatalf("allocated protected addresses: %+v %+v", first, second)
	}
	if _, err := s.Offer(context.Background(), leaseTestClient(3), cfg.Router); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("exhausted protected pool: %v", err)
	}
	reserved := leaseTestOffer(t, s, Client{MAC: "02:00:00:aa:00:01", Hostname: "untrusted"}, netip.Addr{})
	if reserved.Address != r.IP || reserved.Hostname != r.Hostname {
		t.Fatalf("reservation not honored: %+v", reserved)
	}
	if got := leaseTestOffer(t, s, leaseTestClient(9), netip.Addr{}); got.Address != outside.IP {
		t.Fatalf("reservation outside pool failed: %+v", got)
	}
	if _, err := s.Offer(context.Background(), Client{MAC: reserved.MAC, ID: "new-identity"}, netip.Addr{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("different identity stole reserved offer: %v", err)
	}
}

func TestLeaseDeclineQuarantineAndRecovery(t *testing.T) {
	cfg := leaseTestConfig()
	s, path := leaseTestOpen(t, cfg)
	ctx := context.Background()
	c := leaseTestClient(1)
	bound := leaseTestBind(t, s, c)
	before := time.Now().UTC().Truncate(time.Second)
	if err := s.Decline(ctx, c, bound.Address); err != nil {
		t.Fatal(err)
	}
	quarantine := leaseTestList(t, s)[0]
	if quarantine.State != "declined" || quarantine.ExpiresAt.Before(before.Add(10*time.Minute)) {
		t.Fatalf("incorrect quarantine: %+v", quarantine)
	}
	if err := s.Decline(ctx, c, bound.Address); !errors.Is(err, ErrUnknownLease) {
		t.Fatalf("repeated decline extended quarantine: %v", err)
	}
	if err := s.Release(ctx, c, bound.Address); !errors.Is(err, ErrUnknownLease) {
		t.Fatalf("release cleared quarantine: %v", err)
	}
	// The declining client can get a different address while quarantine remains.
	second := leaseTestBind(t, s, c)
	if second.Address == bound.Address {
		t.Fatal("quarantined address immediately reused")
	}
	offer := leaseTestOffer(t, s, leaseTestClient(2), netip.Addr{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ls := leaseTestList(t, s)
	if len(ls) != 3 || ls[0] != quarantine || ls[1] != second || ls[2] != offer {
		t.Fatalf("restart lost durable state: %+v", ls)
	}
	if _, err := s.Offer(ctx, leaseTestClient(3), bound.Address); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("quarantine lost on restart: %v", err)
	}
	s.now = func() time.Time { return quarantine.ExpiresAt }
	reused := leaseTestOffer(t, s, leaseTestClient(3), bound.Address)
	if reused.Address != bound.Address {
		t.Fatalf("quarantine not reused at expiry: %+v", reused)
	}
}

func TestLeaseConcurrentUniqueness(t *testing.T) {
	cfg := leaseTestConfig()
	cfg.RangeEnd = netip.MustParseAddr("192.0.2.73")
	s, _ := leaseTestOpen(t, cfg)
	const clients = 64
	results := make(chan Lease, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for i := 1; i <= clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := leaseTestClient(i)
			l, err := s.Offer(context.Background(), c, cfg.RangeStart)
			if err == nil {
				l, err = s.Acknowledge(context.Background(), c, l.Address, Selecting)
			}
			results <- l
			errs <- err
		}(i)
	}
	wg.Wait()
	seen := map[netip.Addr]bool{}
	for i := 0; i < clients; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		l := <-results
		if seen[l.Address] || l.State != "bound" {
			t.Fatalf("concurrent duplicate or unbound lease: %+v", l)
		}
		seen[l.Address] = true
	}
	if _, err := s.Offer(context.Background(), leaseTestClient(100), netip.Addr{}); !errors.Is(err, ErrNoAddress) {
		t.Fatalf("pool capacity not enforced: %v", err)
	}
	// Duplicate DISCOVERs share one existing lease, even under contention.
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Offer(context.Background(), leaseTestClient(1), netip.Addr{})
			errs <- err
		}()
	}
	wg.Wait()
	for i := 0; i < clients; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if ls := leaseTestList(t, s); len(ls) != clients {
		t.Fatalf("duplicate DISCOVER allocated extra leases: %d", len(ls))
	}
}

func TestLeaseCancellationAndClosedStore(t *testing.T) {
	s, _ := leaseTestOpen(t, leaseTestConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l, err := s.Offer(ctx, leaseTestClient(1), netip.Addr{}); !errors.Is(err, context.Canceled) || l.Address.IsValid() {
		t.Fatalf("canceled offer: %+v %v", l, err)
	}
	if len(leaseTestList(t, s)) != 0 {
		t.Fatal("canceled offer changed database")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(context.Background()); err == nil {
		t.Fatal("closed store accepted operation")
	}
}

func leasePersistent(l Lease) Lease { l.GrantedDuration = 0; return l }
