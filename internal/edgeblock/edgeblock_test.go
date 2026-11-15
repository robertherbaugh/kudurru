package edgeblock

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func id(n int) string { return fmt.Sprintf("%016x", n) }

func TestParsePrefixCanonicalAndBounds(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":       "192.0.2.7/32",
		"192.0.2.7/24":    "192.0.2.0/24",
		" 198.51.0.0/16 ": "198.51.0.0/16",
		"2001:db8::1":     "2001:db8::1/128",
		"2001:db8:1::/48": "2001:db8:1::/48",
		"2001:db8::5/32":  "2001:db8::/32",
	} {
		p, err := ParsePrefix(in)
		if err != nil || p.String() != want {
			t.Errorf("ParsePrefix(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	for _, in := range []string{"", "10.0.0.0/8", "0.0.0.0/0", "::/0", "2001:db8::/31", "::ffff:192.0.2.1", "fe80::1%eth0", "example.com", "192.0.2.0/33", strings.Repeat("1", 65)} {
		if _, err := ParsePrefix(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParsePrefix(%q) err = %v, want ErrInvalid", in, err)
		}
	}
}

func TestAddRevokeLifecycle(t *testing.T) {
	s := Empty()
	s, b, err := Add(s, Request{Prefix: "192.0.2.9/24", Duration: 10 * time.Minute, Reason: "abuse", Actor: "user:ops"}, id(2), t0.Add(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if b.Prefix != "192.0.2.0/24" || !b.CreatedAt.Equal(t0) || !b.Until.Equal(t0.Add(10*time.Minute)) || b.CreatedBy != "user:ops" || s.Revision != 1 {
		t.Fatalf("block = %+v revision %d", b, s.Revision)
	}
	if _, _, err := Add(s, Request{Prefix: "192.0.2.1/24", Duration: time.Hour}, id(3), t0); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate prefix err = %v", err)
	}
	s, _, err = Add(s, Request{Prefix: "2001:db8::/48", Until: t0.Add(time.Hour).Add(time.Millisecond)}, id(1), t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Blocks[0].ID != id(1) || !s.Blocks[0].Until.Equal(t0.Add(time.Hour+time.Second)) || s.Revision != 2 {
		t.Fatalf("blocks not sorted or until not rounded up: %+v", s.Blocks)
	}
	// Expired blocks are pruned by the next change; revoke of one is NotFound.
	later := t0.Add(11 * time.Minute)
	if _, _, err := Revoke(s, id(2), later); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke of expired block err = %v", err)
	}
	s, removed, err := Revoke(s, id(1), later)
	if err != nil || removed.ID != id(1) || len(s.Blocks) != 0 || s.Revision != 3 {
		t.Fatalf("revoke = %+v %v state %+v", removed, err, s)
	}
	if _, _, err := Revoke(s, "nothex", later); !errors.Is(err, ErrNotFound) {
		t.Fatal("malformed id must be not found")
	}
}

func TestAddRefusesOutOfBoundRequests(t *testing.T) {
	cases := []Request{
		{Prefix: "192.0.2.1", Duration: 59 * time.Second},
		{Prefix: "192.0.2.1", Duration: MaxDuration + time.Second},
		{Prefix: "192.0.2.1"},
		{Prefix: "192.0.2.1", Duration: time.Hour, Until: t0.Add(time.Hour)},
		{Prefix: "192.0.2.1", Until: t0.Add(-time.Hour)},
		{Prefix: "192.0.2.1", Duration: time.Hour, Reason: "line\nbreak"},
		{Prefix: "192.0.2.1", Duration: time.Hour, Reason: strings.Repeat("r", MaxReasonBytes+1)},
		{Prefix: "10.0.0.0/8", Duration: time.Hour},
	}
	for _, c := range cases {
		if _, _, err := Add(Empty(), c, id(1), t0); !errors.Is(err, ErrInvalid) {
			t.Errorf("Add(%+v) err = %v, want ErrInvalid", c, err)
		}
	}
}

func TestAddBoundsTheActiveSet(t *testing.T) {
	s := Empty()
	for i := range MaxBlocks {
		var err error
		s, _, err = Add(s, Request{Prefix: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), Duration: time.Hour}, id(i+1), t0)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := Add(s, Request{Prefix: "192.0.2.0/24", Duration: time.Hour}, id(100000), t0); !errors.Is(err, ErrBounds) {
		t.Fatalf("full set err = %v", err)
	}
	// After every block expired, the set has room again.
	if next, _, err := Add(s, Request{Prefix: "192.0.2.0/24", Duration: time.Hour}, id(100000), t0.Add(2*time.Hour)); err != nil || len(next.Blocks) != 1 {
		t.Fatalf("pruned add = %v, %d blocks", err, len(next.Blocks))
	}
	data, err := Encode(s)
	if err != nil || len(data) > MaxDocumentBytes {
		t.Fatalf("full document %d bytes: %v", len(data), err)
	}
}

func TestDecodeRefusesMalformedState(t *testing.T) {
	good, _, _ := Add(Empty(), Request{Prefix: "192.0.2.0/24", Duration: time.Hour}, id(1), t0)
	data, err := Encode(good)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(data)
	if err != nil || back.Revision != 1 || len(back.Blocks) != 1 {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	mutate := func(f func(map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	block := func(m map[string]any) map[string]any { return m["blocks"].([]any)[0].(map[string]any) }
	for name, raw := range map[string][]byte{
		"version":    mutate(func(m map[string]any) { m["formatVersion"] = 2 }),
		"unknown":    mutate(func(m map[string]any) { m["extra"] = true }),
		"prefix":     mutate(func(m map[string]any) { block(m)["prefix"] = "192.0.2.1/24" }),
		"broad":      mutate(func(m map[string]any) { block(m)["prefix"] = "10.0.0.0/8" }),
		"id":         mutate(func(m map[string]any) { block(m)["id"] = "XYZ" }),
		"lifetime":   mutate(func(m map[string]any) { block(m)["until"] = "2026-12-31T00:00:00Z" }),
		"fractional": mutate(func(m map[string]any) { block(m)["until"] = "2026-11-10T12:30:00.5Z" }),
		"duplicate": mutate(func(m map[string]any) {
			b := block(m)
			c := map[string]any{}
			for k, v := range b {
				c[k] = v
			}
			c["id"] = id(2)
			m["blocks"] = []any{b, c}
		}),
		"trailing": append(append([]byte{}, data...), []byte(" {}")...),
		"empty":    nil,
	} {
		if _, err := Decode(raw); !errors.Is(err, ErrState) {
			t.Errorf("%s: Decode err = %v, want ErrState", name, err)
		}
	}
}

func TestOwnerRowsAndImageValidation(t *testing.T) {
	s, _, _ := Add(Empty(), Request{Prefix: "192.0.2.0/24", Duration: time.Hour}, id(1), t0)
	data, _ := Encode(s)
	other := replication.Entry{Key: "inventory/v1/root", Data: json.RawMessage(`{}`)}
	row := replication.Entry{Key: Key, Data: data}
	if err := ValidateEntry(Section, other); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEntry("security", replication.Entry{Key: Key, Data: json.RawMessage(`1`)}); err != nil {
		t.Fatal("other sections are not this owner's")
	}
	if err := ValidateEntry(Section, row); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEntry(Section, replication.Entry{Key: Prefix + "v2/state", Data: data}); !errors.Is(err, ErrState) {
		t.Fatal("unknown owner key accepted")
	}
	got, err := FromRows([]replication.Entry{other})
	if err != nil || len(got.Blocks) != 0 || got.FormatVersion != FormatVersion {
		t.Fatalf("absent owner = %+v, %v", got, err)
	}
	got, err = FromRows([]replication.Entry{other, row})
	if err != nil || len(got.Blocks) != 1 {
		t.Fatalf("present owner = %+v, %v", got, err)
	}
	image := &replication.Image{Complete: true}
	image.Payload.Runtime = []replication.Entry{row, row}
	if err := ValidateImage(image); !errors.Is(err, ErrState) {
		t.Fatal("duplicate owner rows accepted")
	}
	image.Payload.Runtime = []replication.Entry{row}
	if err := ValidateImage(image); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImage(&replication.Image{}); !errors.Is(err, ErrState) {
		t.Fatal("incomplete image accepted")
	}
}
