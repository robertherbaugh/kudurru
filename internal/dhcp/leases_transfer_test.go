package dhcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func transferTestData(t *testing.T, s *LeaseStore, records ...LeaseRecord) []byte {
	t.Helper()
	cfg := s.Configuration().Config
	data, err := json.Marshal(LeaseDocument{Version: 1, Scope: cfg.Name, Subnet: cfg.Subnet, Leases: records})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func transferTestRecord(n int, ip netip.Addr, expires time.Time) LeaseRecord {
	c := leaseTestClient(n)
	return LeaseRecord{Address: ip, MAC: c.MAC, Identity: LeaseIdentity{Kind: "mac", Value: c.MAC}, Hostname: c.Hostname, ExpiresAt: expires, State: "bound"}
}

func TestLeaseTransferNativeLosslessRoundTrip(t *testing.T) {
	cfg := leaseTestConfig()
	cfg.RangeEnd = cfg.RangeEnd.Next()
	s, _ := leaseTestOpen(t, cfg)
	c := leaseTestClient(1)
	leaseTestBind(t, s, c)
	// Identical public ClientID text, distinct internal identity namespaces.
	c.ID = c.MAC
	leaseTestBind(t, s, c)
	q := leaseTestOffer(t, s, leaseTestClient(2), netip.Addr{})
	if err := s.Decline(context.Background(), leaseTestClient(2), q.Address); err != nil {
		t.Fatal(err)
	}
	leaseTestOffer(t, s, leaseTestClient(3), netip.Addr{})
	doc, err := s.Export(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Leases) != 4 || doc.Leases[0].Identity.Kind != "mac" || doc.Leases[1].Identity.Kind != "client-id" || doc.Leases[2].State != "declined" || doc.Leases[3].State != "offered" {
		t.Fatalf("lossy export: %+v", doc)
	}
	target, _ := leaseTestOpen(t, cfg)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	result, err := target.Import(context.Background(), "native", data)
	if err != nil || result.Imported != 4 {
		t.Fatalf("import: %+v %v", result, err)
	}
	if got, want := leaseTestList(t, target), leaseTestList(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip: %+v want %+v", got, want)
	}
	result, err = target.Import(context.Background(), "native", data)
	if err != nil || result.Unchanged != 4 || result.Imported != 0 {
		t.Fatalf("idempotent merge: %+v %v", result, err)
	}
	if _, err := target.Acknowledge(context.Background(), c, doc.Leases[1].Address, Renewing); err != nil {
		t.Fatalf("opaque identity not preserved: %v", err)
	}
}

func TestLeaseTransferMergePreservesBindingAndExpiry(t *testing.T) {
	s, _ := leaseTestOpen(t, leaseTestConfig())
	c := leaseTestClient(1)
	l := leaseTestBind(t, s, c)
	record := transferTestRecord(1, l.Address, l.ExpiresAt.Add(-time.Hour))
	record.State, record.Hostname = "offered", "old-backup"
	result, err := s.Import(context.Background(), "native", transferTestData(t, s, record))
	if err != nil || result.Unchanged != 1 {
		t.Fatalf("merge: %+v %v", result, err)
	}
	if got := leaseTestList(t, s); len(got) != 1 || got[0] != l {
		t.Fatalf("merge shortened or downgraded binding: %+v", got)
	}
	// Expired duplicate rows do not conflict with a live record in the same backup.
	expired := record
	expired.ExpiresAt = time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	result, err = s.Import(context.Background(), "native", transferTestData(t, s, expired, record))
	if err != nil || result.SkippedExpired != 1 || result.Unchanged != 1 {
		t.Fatalf("expired duplicate: %+v %v", result, err)
	}
}

func TestLeaseTransferRejectsConflictsAtomically(t *testing.T) {
	for _, test := range []string{"different-owner", "same-client-two-addresses", "duplicate-input", "quarantine", "reservation", "outside-pool", "invalid-identity", "invalid-hostname", "wrong-scope", "unknown-field", "trailing-document", "write-failure", "commit-failure"} {
		t.Run(test, func(t *testing.T) {
			cfg := leaseTestConfig()
			cfg.Reservations = []Reservation{{MAC: leaseTestClient(9).MAC, IP: cfg.RangeEnd}}
			s, _ := leaseTestOpen(t, cfg)
			l := leaseTestBind(t, s, leaseTestClient(1))
			records := []LeaseRecord{transferTestRecord(2, l.Address.Next(), l.ExpiresAt)}
			switch test {
			case "different-owner":
				records = append(records, transferTestRecord(3, l.Address, l.ExpiresAt))
			case "same-client-two-addresses":
				records = append(records, transferTestRecord(1, cfg.RangeEnd, l.ExpiresAt))
			case "duplicate-input":
				records = append(records, records[0])
			case "quarantine":
				q := transferTestRecord(1, l.Address, l.ExpiresAt)
				q.State = "declined"
				records = append(records, q)
			case "reservation":
				records = append(records, transferTestRecord(3, cfg.RangeEnd, l.ExpiresAt))
			case "outside-pool":
				records = append(records, transferTestRecord(3, netip.MustParseAddr("192.0.2.80"), l.ExpiresAt))
			case "invalid-identity":
				records[0].Identity = LeaseIdentity{Kind: "client-id", Value: "not-base64"}
			case "invalid-hostname":
				records[0].Hostname = "invalid name"
			case "write-failure":
				leaseTestSQL(t, s, `CREATE TRIGGER fail_transfer BEFORE INSERT ON leases BEGIN SELECT RAISE(ABORT,'injected'); END`)
			case "commit-failure":
				leaseTestSQL(t, s, `CREATE TABLE fault_parent(id INTEGER PRIMARY KEY); CREATE TABLE fault_child(parent INTEGER REFERENCES fault_parent(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_transfer AFTER INSERT ON leases BEGIN INSERT INTO fault_child VALUES(1); END`)
			}
			before := leaseTestList(t, s)
			snapshot := s.DNS()
			data := transferTestData(t, s, records...)
			switch test {
			case "wrong-scope":
				data = []byte(strings.Replace(string(data), `"scope":"default"`, `"scope":"other"`, 1))
			case "unknown-field":
				data = append([]byte(`{"unknown":true,`), data[1:]...)
			case "trailing-document":
				data = append(data, []byte(` {}`)...)
			}
			result, err := s.Import(context.Background(), "native", data)
			if err == nil || result != (ImportResult{}) {
				t.Fatalf("rejected import: %+v %v", result, err)
			}
			if s.DNS() != snapshot {
				t.Fatal("failed import published snapshot")
			}
			if got := leaseTestList(t, s); !reflect.DeepEqual(got, before) {
				t.Fatalf("partial merge: %+v want %+v", got, before)
			}
		})
	}
}

func TestLeaseTransferDNSMasqCompatibility(t *testing.T) {
	s, _ := leaseTestOpen(t, leaseTestConfig())
	expiration := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	data := []byte(fmt.Sprintf("# imported\n%d %s 192.0.2.10 host 01:02:03:04:05:06:07\n%d %s 192.0.2.11 * *\n", expiration.Unix(), leaseTestClient(1).MAC, expiration.Unix(), leaseTestClient(2).MAC))
	result, err := s.Import(context.Background(), "dnsmasq", data)
	if err != nil || result.Imported != 2 {
		t.Fatalf("dnsmasq: %+v %v", result, err)
	}
	c := leaseTestClient(1)
	c.ID = "01020304050607"
	if _, err := s.Acknowledge(context.Background(), c, netip.MustParseAddr("192.0.2.10"), Renewing); err != nil {
		t.Fatalf("wire identity cannot renew imported lease: %v", err)
	}
	exported, err := s.ExportDNSMasq(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target, _ := leaseTestOpen(t, leaseTestConfig())
	if _, err := target.Import(context.Background(), "dnsmasq", exported); err != nil {
		t.Fatal(err)
	}
	if got, want := leaseTestList(t, target), leaseTestList(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("dnsmasq roundtrip: %+v want %+v", got, want)
	}
}

func TestLeaseTransferDNSMasqAdvisoryHostname(t *testing.T) {
	for _, tc := range []struct {
		name, hostname, want string
	}{
		{"trailing NULs", "workstation\x00\x00", "workstation"},
		{"unknown", "*", ""},
		{"NULs only", "\x00", ""},
		{"embedded NUL", "work\x00station", ""},
		{"underscore", "ESP_1A2B3C", ""},
		{"non ASCII", "caf\u00e9", ""},
		{"oversized label", strings.Repeat("a", 64), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := leaseTestOpen(t, leaseTestConfig())
			expiration := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
			data := fmt.Sprintf("%d %s 192.0.2.10 %s *\n%d %s 192.0.2.11 ordinary *\n",
				expiration.Unix(), leaseTestClient(1).MAC, tc.hostname,
				expiration.Unix(), leaseTestClient(2).MAC)
			result, err := s.Import(context.Background(), "dnsmasq", []byte(data))
			if err != nil || result.Imported != 2 {
				t.Fatalf("hostname prevented import: %+v %v", result, err)
			}
			leases := leaseTestList(t, s)
			if len(leases) != 2 || leases[0].Hostname != tc.want || leases[1].Hostname != "ordinary" || !leases[0].ExpiresAt.Equal(expiration) {
				t.Fatalf("imported leases: %+v, want first hostname %q", leases, tc.want)
			}
		})
	}
}

func TestLeaseTransferRejectsMalformedAndBoundedInput(t *testing.T) {
	s, _ := leaseTestOpen(t, leaseTestConfig())
	for _, line := range []string{
		"0 02:00:00:00:00:01 192.0.2.10 host *",
		"9223372036854775807 02:00:00:00:00:01 192.0.2.10 host *",
		"9999999999 02:00:00:00:00:01 ::1 host *",
		"9999999999 02:00:00:00:00:01 192.0.2.10 host 0:ab",
		"9999999999 02:00:00:00:00:01 192.0.2.10 host zz:ab",
		"9999999999 02:00:00:00:00:01 192.0.2.10 host 01",
		strings.Repeat("x", 4097),
	} {
		if _, err := s.Import(context.Background(), "dnsmasq", []byte(line)); !errors.Is(err, ErrInvalidImport) {
			t.Fatalf("accepted %q: %v", line, err)
		}
	}
	if _, err := s.Import(context.Background(), "native", make([]byte, MaxImportBytes+1)); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Import(ctx, "native", []byte(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled import: %v", err)
	}
	if len(leaseTestList(t, s)) != 0 {
		t.Fatal("invalid input changed state")
	}
}

func TestLeaseTransferDNSMasqRejectsLossyExport(t *testing.T) {
	for _, kind := range []string{"offer", "quarantine", "opaque-id"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := leaseTestOpen(t, leaseTestConfig())
			c := leaseTestClient(1)
			if kind == "opaque-id" {
				c.ID = "opaque-client"
			}
			l := leaseTestOffer(t, s, c, netip.Addr{})
			if kind == "opaque-id" {
				if _, err := s.Acknowledge(context.Background(), c, l.Address, Selecting); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "quarantine" {
				if err := s.Decline(context.Background(), c, l.Address); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ExportDNSMasq(context.Background()); !errors.Is(err, ErrUnsupportedExport) {
				t.Fatalf("lossy export: %v", err)
			}
		})
	}
}

func TestLeaseTransferConcurrentOwnershipAndConfiguration(t *testing.T) {
	s, _ := leaseTestOpen(t, managedTestConfig())
	cfg := s.Configuration().Config
	expires := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 9)
	for n := 1; n <= 8; n++ {
		data := transferTestData(t, s, transferTestRecord(n, cfg.RangeStart, expires))
		wg.Go(func() { _, err := s.Import(context.Background(), "native", data); errs <- err })
	}
	opts := optionsFor(cfg)
	opts.RangeStart = opts.RangeStart.Next()
	wg.Go(func() { errs <- s.UpdateOptions(context.Background(), 1, opts) })
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("conflicting import/configuration commits: %d", successes)
	}
	state := s.Configuration()
	for _, l := range leaseTestList(t, s) {
		if !state.Config.inPool(l.Address) {
			t.Fatal("configuration raced a live import")
		}
	}
}

func TestLeaseTransferClientIdentityEncoding(t *testing.T) {
	s, _ := leaseTestOpen(t, leaseTestConfig())
	record := transferTestRecord(1, s.Configuration().Config.RangeStart, time.Now().UTC().Truncate(time.Second).Add(time.Hour))
	record.Identity = LeaseIdentity{Kind: "client-id", Value: base64.StdEncoding.EncodeToString([]byte("\x00opaque\xff"))}
	if _, err := s.Import(context.Background(), "native", transferTestData(t, s, record)); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Export(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Leases[0].Identity != record.Identity {
		t.Fatalf("opaque bytes altered: %+v", doc.Leases[0].Identity)
	}
}
