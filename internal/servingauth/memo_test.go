package servingauth

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func memoFloorRows(t *testing.T, n int) []replication.Entry {
	t.Helper()
	s := EmptySecurity(1)
	for i := range n {
		s.GrantFloors = append(s.GrantFloors, GrantFloor{FormatVersion: 1, Grant: fmt.Sprintf("viewer@zone:memo-%d", i), ValidAfter: time.Date(2026, 11, 26, 0, 0, i, 0, time.UTC)})
	}
	s.AccountFloors = append(s.AccountFloors, AccountFloor{FormatVersion: 1, Account: AccountRef{Kind: "LocalUser", Name: "alice", UID: "uid-1"}, ValidAfter: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)})
	s.ProviderFloors = append(s.ProviderFloors, ProviderFloor{FormatVersion: 1, Provider: "corp", UID: "uid-2", ValidAfter: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)})
	rows, err := EncodeSecurity(s)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A remembered row returns what validating or decoding it returned; a row
// that is refused is never remembered, so it is refused every time, and a
// remembered floor is a value the caller may change freely.
func TestSecurityRowMemoRemembersOnlyAcceptedRows(t *testing.T) {
	rows := memoFloorRows(t, 3)
	first, err := DecodeSecurity(rows)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DecodeSecurity(rows)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("a remembered decode differs", err)
	}
	for i := range first.GrantFloors {
		first.GrantFloors[i].Grant = "changed by the caller"
	}
	first.AccountFloors[0].Account.Name = "changed by the caller"
	first.ProviderFloors[0].UID = "changed by the caller"
	again, err := DecodeSecurity(rows)
	if err != nil || !reflect.DeepEqual(second, again) {
		t.Fatal("a caller's change reached a remembered floor", err)
	}
	for _, row := range rows {
		if err := ValidateEntry("security", row); err != nil {
			t.Fatal(row.Key, err)
		}
	}
	for i, row := range rows {
		if row.Key == RootKey {
			continue
		}
		// The same key with other bytes: a version this owner refuses, then
		// a member name in another case. Both are refused on every attempt,
		// before and after the accepted bytes were remembered.
		for _, data := range []string{
			`{"formatVersion":2}`,
			`{"FormatVersion":1,"grant":"viewer@zone:memo-0","validAfter":"2026-11-26T00:00:00Z"}`,
		} {
			bad := replication.Entry{Key: row.Key, Data: []byte(data)}
			for range 2 {
				if ValidateEntry("security", bad) == nil {
					t.Fatalf("row %d %s: refused bytes were accepted", i, data)
				}
			}
			tampered := append([]replication.Entry(nil), rows...)
			tampered[i] = bad
			for range 2 {
				if _, err := DecodeSecurity(tampered); err == nil {
					t.Fatalf("row %d %s: a section with refused bytes decoded", i, data)
				}
			}
		}
	}
	// A refused version still answers ErrVersion, as before the memo.
	for i, row := range rows {
		if row.Key == RootKey {
			continue
		}
		tampered := append([]replication.Entry(nil), rows...)
		tampered[i] = replication.Entry{Key: row.Key, Data: []byte(`{"formatVersion":2}`)}
		if _, err := DecodeSecurity(tampered); !errors.Is(err, ErrVersion) {
			t.Fatalf("row %d: %v, want the version refusal", i, err)
		}
	}
}

func TestSecurityRowMemoStaysWithinItsBounds(t *testing.T) {
	c := newMemo(4, 3*10, func(int) int { return 10 })
	for i := range 100 {
		c.put(sha256.Sum256([]byte(fmt.Sprint(i))), i)
		if len(c.m) > 4 || c.bytes > 30 {
			t.Fatalf("after %d entries: %d entries, %d bytes", i+1, len(c.m), c.bytes)
		}
	}
	if v, ok := c.get(sha256.Sum256([]byte("99"))); !ok || v != 99 {
		t.Fatal("the newest entry was not kept")
	}
	big := newMemo(4, 5, func(int) int { return 10 })
	big.put(sha256.Sum256([]byte("x")), 1)
	if len(big.m) != 0 {
		t.Fatal("a value above the byte bound was stored")
	}
}
