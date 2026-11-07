package zonestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

func testNow() time.Time { return time.Unix(1_800_000_000, 0) }

func fixture(t testing.TB) *Aggregate {
	t.Helper()
	a := &Aggregate{FormatVersion: zonekinds.FormatVersion, Zone: zonekinds.ZoneID{ClusterID: "cluster", ObjectName: "example", UID: "zone-uid", Apex: "example."}, Config: json.RawMessage(`{"zoneName":"example.","type":"primary"}`), Policy: Policy{Enabled: true, AuthEpoch: 1, Credentials: []dynamicupdate.Credential{{Key: "key.", Version: "v1"}}, Grants: []dynamicupdate.Grant{{Key: "key.", Owner: "*.example."}}}}
	for _, text := range []string{"example. 300 IN SOA ns.example. hostmaster.example. 10 60 30 600 30", "example. 300 IN NS ns.example.", "ns.example. 300 IN A 192.0.2.1", "unknown.example. 200 IN TYPE65200 \\# 3 AbCdEF"} {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		r, err := FromRR(rr, Provenance{SourceID: "system"})
		if err != nil {
			t.Fatal(err)
		}
		a.Records = append(a.Records, r)
	}
	return a
}
func readPrepared(p *Prepared, l Limits) (*Aggregate, error) {
	return Decode(p.Head, func(d string) (*zonekinds.ZoneChunk, error) {
		for _, c := range p.Chunks {
			if c.Spec.Digest == d {
				return c, nil
			}
		}
		return nil, ErrInvalid
	}, l)
}

func TestCodecDeterministicDetachedAndMetadataSerial(t *testing.T) {
	a := fixture(t)
	p, err := Prepare(a, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	b := *a
	b.Records = append([]Record(nil), a.Records...)
	for i, j := 0, len(b.Records)-1; i < j; i, j = i+1, j-1 {
		b.Records[i], b.Records[j] = b.Records[j], b.Records[i]
	}
	b.Config = json.RawMessage(`{ "type": "primary", "zoneName": "example." }`)
	other, err := Prepare(&b, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Head.Spec.RootDigest != other.Head.Spec.RootDigest || !reflect.DeepEqual(p.Chunks, other.Chunks) {
		t.Fatal("encoding depends on input order/JSON whitespace")
	}
	decoded, err := readPrepared(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	decoded.Records[0].Wire[0] ^= 1
	if !reflect.DeepEqual(p.Chunks, other.Chunks) {
		t.Fatal("decode aliases prepared data")
	}
	b.Config = json.RawMessage(`{"zoneName":"example.","type":"primary","queryAccess":"deny"}`)
	metadata, err := Prepare(&b, p.Head, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Head.Spec.Serial != 10 || metadata.Head.Spec.ContentHash != p.Head.Spec.ContentHash || metadata.Head.Spec.CommitSequence != 2 || metadata.Head.Spec.NotifySequence != 0 {
		t.Fatal("metadata advanced serial/notify")
	}
	b.Policy.Enabled = false
	if _, err := Prepare(&b, p.Head, Limits{}); err == nil {
		t.Fatal("policy changed without auth barrier")
	}
	b.Policy.AuthEpoch++
	if _, err := Prepare(&b, p.Head, Limits{}); err != nil {
		t.Fatal(err)
	}
}

func TestCodecRejectsCorruptIncompleteAndAmbiguousData(t *testing.T) {
	a := fixture(t)
	p, err := Prepare(a, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Prepared)
	}{
		{"missing", func(p *Prepared) { p.Chunks = p.Chunks[:1] }},
		{"corrupt", func(p *Prepared) { p.Chunks[0].Spec.Data[0] ^= 1 }},
		{"wrong serial", func(p *Prepared) { p.Head.Spec.Serial++ }},
		{"wrong UID", func(p *Prepared) {
			p.Head.Spec.Zone.UID = "recreated"
			p.Head.Name = zonekinds.HeadName(p.Head.Spec.Zone)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := &Prepared{Head: p.Head.DeepCopy()}
			for _, c := range p.Chunks {
				cp.Chunks = append(cp.Chunks, c.DeepCopy())
			}
			tc.change(cp)
			if _, err := VerifyPrepared(cp, Limits{}); err == nil {
				t.Fatal("invalid preparation accepted")
			}
		})
	}
	a.Config = json.RawMessage(`{"type":"primary","type":"secondary"}`)
	if _, err := Prepare(a, nil, Limits{}); err == nil {
		t.Fatal("duplicate keys accepted")
	}
	if _, err := Prepare(fixture(t), nil, Limits{MaxRecords: 2}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("record bound %v", err)
	}
	if _, err := Prepare(fixture(t), nil, Limits{MaxWireBytes: 10}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("wire bound %v", err)
	}
	if _, err := Prepare(fixture(t), nil, Limits{MaxAggregateBytes: 50}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("aggregate bound %v", err)
	}
}

func TestCodecLargeZoneBoundedChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("100k-record codec acceptance")
	}
	a := fixture(t)
	for i := 0; i < 100_000; i++ {
		rr, err := dns.NewRR(fmt.Sprintf("r%d.example. 60 IN A 192.0.2.8", i))
		if err != nil {
			t.Fatal(err)
		}
		r, err := FromRR(rr, Provenance{SourceID: "system"})
		if err != nil {
			t.Fatal(err)
		}
		a.Records = append(a.Records, r)
	}
	p, err := Prepare(a, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Chunks) < 10 {
		t.Fatal("large zone did not exercise chunking")
	}
	for _, c := range p.Chunks {
		if len(c.Spec.Data) > zonekinds.MaxChunkBytes {
			t.Fatal("oversized chunk")
		}
	}
	decoded, err := readPrepared(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Records) != 100_004 {
		t.Fatal("large zone incomplete")
	}
	t.Logf("100004 records: %d immutable chunks, max payload %d bytes, serial %d", len(p.Chunks), zonekinds.MaxChunkBytes, p.Head.Spec.Serial)
	if _, err := Prepare(a, nil, Limits{MaxChunks: 2}); !errors.Is(err, ErrCapacity) {
		t.Fatal("chunk bound bypassed")
	}
}

func TestReservationsAndDesiredTakeover(t *testing.T) {
	a := fixture(t)
	rr, _ := dns.NewRR("dynamic.example. 60 IN A 192.0.2.7")
	r, err := FromRR(rr, Provenance{Key: "key.", CredentialVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	a.Records = append(a.Records, r)
	p := Projection{Source: Source{ID: "file:test", UID: "uid", ResourceVersion: "rv", Manager: "file:test", Desired: json.RawMessage(`{}`)}, Reservations: []dynamicupdate.Reservation{{Name: "dynamic.example.", Type: dns.TypeA}}}
	if _, err := ReplaceSource(a, p, testNow(), Limits{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("dynamic conflict silently replaced: %v", err)
	}
	p.Takeover = true
	b, err := ReplaceSource(a, p, testNow(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range b.Records {
		if strings.Contains(string(r.Wire), "dynamic") {
			t.Fatal("takeover retained conflicting dynamic RR")
		}
	}
	if len(a.Records) != 5 || len(a.Sources) != 0 {
		t.Fatal("projection mutated original")
	}
	v := &View{Aggregate: b}
	state, err := v.UpdateState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Reservations) < len(b.Reservations)+len(b.Records) {
		t.Fatal("static records failed to reserve their identities")
	}
}
