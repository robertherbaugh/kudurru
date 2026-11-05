package accepted

import (
	"bytes"
	"crypto"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

func signedState(t *testing.T, cluster string) (trustanchors.State, trustanchors.Observation) {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal("generate real signing key")
	}
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 3600}, Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: ".", Inception: uint32(time.Now().Add(-time.Hour).Unix() & 0xffffffff), Expiration: uint32(time.Now().Add(time.Hour).Unix() & 0xffffffff)}
	if err := sig.Sign(private.(crypto.Signer), []dns.RR{key}); err != nil {
		t.Fatal("sign real root RRset")
	}
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	m.Answer = []dns.RR{key, sig}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	s, o, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: cluster, PolicyID: "operator-selected-seed", DS: []string{key.ToDS(dns.SHA256).String()}}, m, clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	return s, o
}

func ownerFixture(t *testing.T) (Snapshot, Snapshot) {
	t.Helper()
	dormant, err := NewDormant("cluster", strings.Repeat("1", 32), time.Now().Add(-time.Second).UTC())
	if err != nil {
		t.Fatal(err)
	}
	state, observation := signedState(t, "cluster")
	managed, err := ProposeManaged(dormant, state, time.Now().UTC(), observation.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	return dormant, managed
}

func TestOwnerRoundtripBothVersionsAndDetachedState(t *testing.T) {
	dormant, managed := ownerFixture(t)
	for _, original := range []Snapshot{dormant, managed} {
		rows, err := EncodeRows(original)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, replication.Entry{Key: "foreign/state", Data: json.RawMessage(`{"value":true}`)})
		decoded, err := DecodeRows(rows, "cluster")
		if err != nil || decoded.Version() != original.Version() {
			t.Fatal("roundtrip changed complete versions", err)
		}
		if err := ValidateImage(&replication.Image{Term: replication.Term{ClusterID: "cluster"}, CreatedAt: time.Now().UTC(), Payload: replication.Payload{Runtime: rows}}); err != nil {
			t.Fatal(err)
		}
		before := decoded.Version()
		inspect := decoded.Inspect()
		inspect.ClusterID = "other"
		if len(inspect.Chunks) != 0 {
			inspect.Chunks[0].Digest = strings.Repeat("0", 64)
		}
		if decoded.Version() != before || decoded.Inspect().ClusterID != "cluster" {
			t.Fatal("inspection mutated owner")
		}
	}
	if dormant.Version().Pure != (PureVersion{}) || managed.Version().Generation != 2 || managed.Version().Pure.Generation != 1 {
		t.Fatal("envelope and pure generation conflated")
	}
	state, err := managed.State()
	if err != nil {
		t.Fatal(err)
	}
	doc := state.Inspect()
	doc.Keys[0].Support = append(doc.Keys[0].Support, "untrusted")
	if _, err := trustanchors.Encode(state); err != nil {
		t.Fatal("diagnostic state mutation escaped")
	}
	if _, err := dormant.State(); err == nil {
		t.Fatal("dormant acquired pure authority")
	}
	if _, err := ProposeManaged(managed, state, time.Now().UTC(), time.Now().Add(time.Hour)); !errors.Is(err, ErrCAS) {
		t.Fatal("replayed pure generation accepted")
	}
}

func TestOwnerStrictCompleteShapes(t *testing.T) {
	_, managed := ownerFixture(t)
	good, err := EncodeRows(managed)
	if err != nil {
		t.Fatal(err)
	}
	clone := func() []replication.Entry {
		rows := slices.Clone(good)
		for i := range rows {
			rows[i].Data = bytes.Clone(rows[i].Data)
		}
		return rows
	}
	for _, mode := range []string{"missing-root", "missing-chunk", "duplicate", "orphan", "unknown-owner", "unknown-field", "null", "alias", "duplicate-field", "trailing", "wrong-pure", "wrong-bytes", "unknown-phase", "history", "unicode", "oversized", "sentinel"} {
		t.Run(mode, func(t *testing.T) {
			rows := clone()
			rootIndex := slices.IndexFunc(rows, func(row replication.Entry) bool { return row.Key == RootKey })
			switch mode {
			case "missing-root":
				rows = slices.Delete(rows, rootIndex, rootIndex+1)
			case "missing-chunk":
				rows = slices.Delete(rows, 0, 1)
			case "duplicate":
				rows = append(rows, rows[0])
			case "orphan":
				row, err := encodeRow(ChunkPrefix+digest([]byte("orphan")), Chunk{FormatVersion: 1, Digest: digest([]byte("orphan")), Data: []byte("orphan")})
				if err != nil {
					t.Fatal(err)
				}
				rows = append(rows, row)
			case "unknown-owner":
				rows = append(rows, replication.Entry{Key: Prefix + "v2/root", Data: json.RawMessage(`{}`)})
			case "unknown-field":
				rows[rootIndex].Data = append([]byte(`{"extra":1,`), rows[rootIndex].Data[1:]...)
			case "null":
				rows[rootIndex].Data = bytes.Replace(rows[rootIndex].Data, []byte(`"trustPoint":"."`), []byte(`"trustPoint":null`), 1)
			case "alias":
				rows[rootIndex].Data = bytes.Replace(rows[rootIndex].Data, []byte(`"trustPoint"`), []byte(`"TrustPoint"`), 1)
			case "duplicate-field":
				rows[rootIndex].Data = append([]byte(`{"trustPoint":".",`), rows[rootIndex].Data[1:]...)
			case "trailing":
				rows[rootIndex].Data = append(rows[rootIndex].Data, []byte(` {}`)...)
			case "wrong-pure":
				r := managed.Inspect()
				r.Pure.Digest = strings.Repeat("0", 64)
				rows[rootIndex], err = encodeRow(RootKey, r)
			case "wrong-bytes":
				r := managed.Inspect()
				r.StateBytes++
				rows[rootIndex], err = encodeRow(RootKey, r)
			case "unknown-phase":
				rows[rootIndex].Data = bytes.ReplaceAll(rows[rootIndex].Data, []byte(`"managed"`), []byte(`"static"`))
			case "history":
				rows[rootIndex].Data = bytes.ReplaceAll(rows[rootIndex].Data, []byte(strings.Repeat("1", 32)), []byte("history"))
			case "unicode":
				rows[rootIndex].Data = bytes.Replace(rows[rootIndex].Data, []byte(`"operator-selected-seed"`), []byte(`"\ud800"`), 1)
			case "oversized":
				rows[rootIndex].Data = bytes.Repeat([]byte(" "), MaxRootBytes+1)
			case "sentinel":
				rows = []replication.Entry{{Key: RootKey, Data: json.RawMessage(AwaitingOwnerJSON)}}
			}
			if err != nil {
				t.Fatal("prepare structural fixture", err)
			}
			if _, err := DecodeRows(rows, "cluster"); err == nil {
				t.Fatal("invalid complete owner admitted")
			}
		})
	}
	if _, err := DecodeRows(good, "another-cluster"); !errors.Is(err, replication.ErrWrongCluster) {
		t.Fatal("cluster substitution accepted")
	}
	if _, err := DecodeRows(nil, "cluster"); !errors.Is(err, ErrMissing) {
		t.Fatal("missing history became dormant")
	}
}

func TestSentinelAndAllocationBounds(t *testing.T) {
	sentinel := []replication.Entry{{Key: RootKey, Data: json.RawMessage(AwaitingOwnerJSON)}}
	if !IsAwaitingOwner(sentinel) {
		t.Fatal("exact non-granting sentinel not recognized")
	}
	if err := ValidateEntry(Section, sentinel[0]); err == nil {
		t.Fatal("sentinel admitted as normal authority")
	}
	if IsAwaitingOwner(append(slices.Clone(sentinel), replication.Entry{Key: ChunkPrefix + strings.Repeat("0", 64), Data: json.RawMessage(`{}`)})) {
		t.Fatal("orphan sentinel accepted")
	}
	rows := make([]replication.Entry, MaxRows+1)
	for i := range rows {
		rows[i] = replication.Entry{Key: Prefix + strings.Repeat("x", i+1), Data: json.RawMessage(`{}`)}
	}
	if _, err := DecodeRows(rows, "cluster"); !errors.Is(err, ErrBounds) {
		t.Fatal("row allocation not bounded")
	}
	if _, err := NewDormant("cluster", strings.Repeat("f", 31), time.Now().UTC()); err == nil {
		t.Fatal("malformed history accepted")
	}
	if err := ValidateEntry("runtime", replication.Entry{Key: "foreign/state", Data: json.RawMessage(`null`)}); err != nil {
		t.Fatal("foreign owner interpreted")
	}
}

func TestOwnerMultichunkPermanentHistoryAndExactPureDigest(t *testing.T) {
	dormant, initial := ownerFixture(t)
	state, err := initial.State()
	if err != nil {
		t.Fatal(err)
	}
	doc := state.Inspect()
	support := doc.Keys[0].ID
	// Structural codec fixture only: decoding never authenticates these claimed
	// transitions. The concrete adapter cannot accept a caller-built next State.
	for range trustanchors.MaxTombstones {
		key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
		if _, err := key.Generate(256); err != nil {
			t.Fatal("generate valid point fixture")
		}
		id := digest([]byte(key.String()))
		key.Flags |= dns.REVOKE
		doc.Keys = append(doc.Keys, trustanchors.Key{ID: id, DNSKEY: key.String(), Status: trustanchors.Revoked, FirstSeen: doc.Checkpoint.Wall, LastSeen: doc.Checkpoint.Wall, OriginalTTL: 3600, Support: []string{support}, RevokedAt: doc.Checkpoint.Wall})
	}
	slices.SortFunc(doc.Keys, func(a, b trustanchors.Key) int { return strings.Compare(a.ID, b.ID) })
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	large, err := trustanchors.Decode(data)
	if err != nil {
		t.Fatal("validate full bounded tombstone fixture", err)
	}
	managed, err := ProposeManaged(dormant, large, time.Now().UTC(), time.Now().Add(time.Hour).UTC())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := EncodeRows(managed)
	if err != nil {
		t.Fatal(err)
	}
	if len(managed.Inspect().Chunks) < 2 {
		t.Fatal("fixture did not cross raw chunk boundary")
	}
	decoded, err := DecodeRows(rows, "cluster")
	if err != nil || decoded.Version() != managed.Version() {
		t.Fatal("multirow history lost", err)
	}
	restored, err := decoded.State()
	if err != nil || len(restored.Inspect().Keys) != trustanchors.MaxTombstones+1 {
		t.Fatal("tombstones pruned by codec", err)
	}
	for _, row := range rows {
		if len(row.Data) > MaxRowBytes || (row.Key == RootKey && len(row.Data) > MaxRootBytes) {
			t.Fatal("encoded row exceeded budget")
		}
	}
	// Preserve the same semantic pure document but change its serialized order.
	// The envelope must still bind the pure package's exact canonical Version.
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(reordered, data) {
		t.Fatal("fixture did not change serialized digest")
	}
	r := managed.Inspect()
	r.Pure.Digest, r.StateBytes, r.Chunks = digest(reordered), len(reordered), []ChunkRef{}
	for offset := 0; offset < len(reordered); offset += MaxChunkBytes {
		part := reordered[offset:min(offset+MaxChunkBytes, len(reordered))]
		r.Chunks = append(r.Chunks, ChunkRef{Digest: digest(part), Bytes: len(part)})
	}
	wrongRows, err := rowsFor(r, reordered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRows(wrongRows, "cluster"); err == nil {
		t.Fatal("serialized hash substituted for exact pure version")
	}
}
