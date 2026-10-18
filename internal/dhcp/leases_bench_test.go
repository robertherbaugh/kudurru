package dhcp

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkLeaseScale uses a real private WAL database with synchronous=FULL.
// Fixture seeding is outside the timer; each operation uses the public API.
// Run: go test ./internal/dhcp -run '^$' -bench '^BenchmarkLeaseScale$' -benchtime=20x -benchmem -count=3
func BenchmarkLeaseScale(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		for _, operation := range []string{"allocate", "renew", "list", "export", "contended-renew"} {
			b.Run(fmt.Sprintf("%d/%s", size, operation), func(b *testing.B) {
				cfg := leaseTestConfig()
				cfg.Subnet = netip.MustParsePrefix("10.77.0.0/16")
				cfg.ServerIP = netip.MustParseAddr("10.77.0.1")
				cfg.Router = netip.MustParseAddr("10.77.0.254")
				cfg.DNSServers = []netip.Addr{cfg.ServerIP}
				cfg.RangeStart = netip.MustParseAddr("10.77.1.0")
				cfg.RangeEnd = netip.MustParseAddr("10.77.254.254")
				cfg.DomainName = "leases.test"
				cfg.RegisterDNS = true
				cfg.DNSTTL = 30 * time.Second
				s, err := OpenLeases(filepath.Join(b.TempDir(), "leases.db"), cfg)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = s.Close() })
				ctx := context.Background()
				clients := make([]Client, size)
				addresses := make([]netip.Addr, size)
				err = s.transact(ctx, func(tx *sql.Tx, _ []storedLease, now time.Time) error {
					ctx = s.transactionContext(ctx)
					ip := cfg.RangeStart
					for i := range clients {
						clients[i] = leaseTestClient(i + 1)
						addresses[i] = ip
						c, key, e := normalizeClient(clients[i])
						if e != nil {
							return e
						}
						if e = saveLease(ctx, tx, storedLease{Lease: Lease{Address: ip, ClientID: clientID(c), MAC: c.MAC, Hostname: c.Hostname, ExpiresAt: now.Add(24 * time.Hour), State: "bound"}, key: key}); e != nil {
							return e
						}
						ip = ip.Next()
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				extra := leaseTestClient(size + 1)
				var counter atomic.Uint64
				b.ReportAllocs()
				b.ResetTimer()
				if operation == "contended-renew" {
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							i := (counter.Add(1) - 1) % uint64(len(clients))
							if _, err := s.Acknowledge(ctx, clients[i], addresses[i], Renewing); err != nil {
								b.Error(err)
							}
						}
					})
					return
				}
				for i := 0; i < b.N; i++ {
					switch operation {
					case "allocate":
						l, e := s.Offer(ctx, extra, netip.Addr{})
						if e != nil {
							b.Fatal(e)
						}
						if _, e = s.Acknowledge(ctx, extra, l.Address, Selecting); e != nil {
							b.Fatal(e)
						}
						if e = s.Release(ctx, extra, l.Address); e != nil {
							b.Fatal(e)
						}
					case "renew":
						_, err = s.Acknowledge(ctx, clients[i%size], addresses[i%size], Renewing)
					case "list":
						_, err = s.List(ctx)
					case "export":
						_, err = s.Export(ctx)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
