package persistence

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

var testAt = time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC)

func client(id, ip string, at time.Time) inventory.Client {
	return inventory.Client{ID: id, Name: id, Hostname: id + ".test", IPs: []netip.Addr{netip.MustParseAddr(ip)}, ObservedAt: at, ExpiresAt: at.Add(time.Hour)}
}
func fixtureState(t *testing.T) inventory.State {
	t.Helper()
	h := inventory.New(func() time.Time { return testAt })
	if err := h.Replace("source/uid-a", 1, []inventory.Client{client("older", "192.0.2.1", testAt.Add(-time.Minute)), client("manual", "192.0.2.2", testAt)}); err != nil {
		t.Fatal(err)
	}
	if err := h.Replace("source/uid-b", 1, []inventory.Client{client("newer", "192.0.2.1", testAt)}); err != nil {
		t.Fatal(err)
	}
	s, err := h.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func encoded(t *testing.T, s inventory.State) []replication.Entry {
	t.Helper()
	rows, err := Encode(s, "cluster-a", testAt)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func copyRows(rows []replication.Entry) []replication.Entry {
	out := make([]replication.Entry, len(rows))
	for i, row := range rows {
		out[i] = replication.Entry{Key: row.Key, Data: bytes.Clone(row.Data)}
	}
	return out
}
func rootOf(t *testing.T, rows []replication.Entry) Root {
	t.Helper()
	for _, row := range rows {
		if row.Key == RootKey {
			var root Root
			if err := json.Unmarshal(row.Data, &root); err != nil {
				t.Fatal(err)
			}
			return root
		}
	}
	t.Fatal("missing test root")
	return Root{}
}
func replaceRoot(t *testing.T, rows []replication.Entry, root Root) {
	t.Helper()
	row, err := encodeRow(RootKey, root)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i].Key == RootKey {
			rows[i] = row
			return
		}
	}
	t.Fatal("missing test root")
}

func TestCodecPreservesFiniteEvidenceAndNullArrays(t *testing.T) {
	s := fixtureState(t)
	s, err := inventory.PrepareReplacement(s, "source/uid-b", 2, nil, testAt)
	if err != nil {
		t.Fatal(err)
	}
	rows := encoded(t, s)
	rows = append(rows, replication.Entry{Key: "foreign/runtime", Data: json.RawMessage(`{"private":true}`)})
	got, err := Decode(rows, "cluster-a", testAt.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !sameState(t, s, got.State) || !got.UpdatedAt.Equal(testAt) || got.Version.Generation != s.Generation {
		t.Fatal("durable restore renewed/changed evidence")
	}
	h, err := inventory.RestoreState(got.State, func() time.Time { return testAt.Add(72 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(netip.MustParseAddr("192.0.2.1"), testAt.Add(time.Minute)) != nil {
		t.Fatal("old owner resurrected behind removal")
	}
	original := copyRows(rows)
	got.State.Watermarks[0].ID = "changed"
	got.State.Sources[0].Clients[0].IPs[0] = netip.MustParseAddr("192.0.2.99")
	if !reflect.DeepEqual(original, rows) {
		t.Fatal("decoded state aliases rows")
	}
	for _, empty := range []inventory.State{{FormatVersion: 1}, {FormatVersion: 1, Sources: []inventory.SourceState{}, Watermarks: []inventory.Watermark{}}} {
		out, err := Decode(encoded(t, empty), "cluster-a", testAt)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(empty, out.State) {
			t.Fatal("null/empty original arrays changed")
		}
	}
	// A source with no devices can historically encode null clients/ips.
	h = inventory.New(func() time.Time { return testAt })
	if err := h.Replace("empty", 1, []inventory.Client{{ID: "no-address", ObservedAt: testAt, ExpiresAt: testAt.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	s, err = h.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(encoded(t, s), "cluster-a", testAt); err != nil {
		t.Fatal(err)
	}
	s.Sources[0].Clients = nil
	if _, err := Decode(encoded(t, s), "cluster-a", testAt); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteOwnerRejectsMissingMixedAndTamperedRows(t *testing.T) {
	good := encoded(t, fixtureState(t))
	for name, mutate := range map[string]func([]replication.Entry) []replication.Entry{
		"missing root":    func(rows []replication.Entry) []replication.Entry { return rows[:len(rows)-1] },
		"missing chunk":   func(rows []replication.Entry) []replication.Entry { return rows[len(rows)-1:] },
		"duplicate root":  func(rows []replication.Entry) []replication.Entry { return append(rows, rows[len(rows)-1]) },
		"duplicate chunk": func(rows []replication.Entry) []replication.Entry { return append(rows, rows[0]) },
		"unknown version": func(rows []replication.Entry) []replication.Entry {
			return append(rows, replication.Entry{Key: "inventory/v2/root", Data: json.RawMessage(`{}`)})
		},
		"unknown key": func(rows []replication.Entry) []replication.Entry {
			return append(rows, replication.Entry{Key: "inventory/v1/other", Data: json.RawMessage(`{}`)})
		},
		"orphan": func(rows []replication.Entry) []replication.Entry {
			data := []byte("orphan")
			row, err := encodeRow(ChunkPrefix+digest(data), Chunk{FormatVersion: 1, Digest: digest(data), Data: data})
			if err != nil {
				t.Fatal(err)
			}
			return append(rows, row)
		},
		"wrong cluster": func(rows []replication.Entry) []replication.Entry {
			root := rootOf(t, rows)
			root.ClusterID = "other"
			replaceRoot(t, rows, root)
			return rows
		},
		"future admission": func(rows []replication.Entry) []replication.Entry {
			root := rootOf(t, rows)
			root.UpdatedAt = testAt.Add(time.Nanosecond)
			replaceRoot(t, rows, root)
			return rows
		},
		"state hash": func(rows []replication.Entry) []replication.Entry {
			root := rootOf(t, rows)
			root.StateDigest = strings.Repeat("0", 64)
			replaceRoot(t, rows, root)
			return rows
		},
		"state generation": func(rows []replication.Entry) []replication.Entry {
			root := rootOf(t, rows)
			root.Generation++
			replaceRoot(t, rows, root)
			return rows
		},
		"chunk reference length": func(rows []replication.Entry) []replication.Entry {
			root := rootOf(t, rows)
			root.Chunks[0].Bytes++
			root.StateBytes++
			replaceRoot(t, rows, root)
			return rows
		},
		"changed chunk bytes": func(rows []replication.Entry) []replication.Entry {
			var chunk Chunk
			if err := json.Unmarshal(rows[0].Data, &chunk); err != nil {
				t.Fatal(err)
			}
			chunk.Data[0] ^= 1
			data, err := json.Marshal(chunk)
			if err != nil {
				t.Fatal(err)
			}
			rows[0].Data = data
			return rows
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(mutate(copyRows(good)), "cluster-a", testAt); err == nil {
				t.Fatal("accepted incomplete/corrupt authority")
			}
		})
	}
	if _, err := Decode(nil, "cluster-a", testAt); !errors.Is(err, ErrUninitialized) {
		t.Fatal(err)
	}
	if err := ValidateImage(&replication.Image{Term: replication.Term{ClusterID: "cluster-a"}, CreatedAt: testAt}); !errors.Is(err, ErrUninitialized) {
		t.Fatal("missing owner-enabled root accepted", err)
	}
	if err := ValidateImage(nil); err == nil {
		t.Fatal("nil image accepted")
	}
	if err := ValidateEntry("security", replication.Entry{Key: RootKey, Data: json.RawMessage(`bad`)}); err != nil {
		t.Fatal("foreign section claimed", err)
	}
}

func TestStrictHistoricalEnvelope(t *testing.T) {
	rows := encoded(t, fixtureState(t))
	root := rows[len(rows)-1]
	chunk := rows[0]
	for name, entry := range map[string]replication.Entry{
		"root missing admission": {Key: RootKey, Data: json.RawMessage(`{"formatVersion":1}`)},
		"duplicate":              {Key: RootKey, Data: bytes.Replace(root.Data, []byte(`"formatVersion":1`), []byte(`"formatVersion":1,"formatVersion":1`), 1)},
		"case":                   {Key: RootKey, Data: bytes.Replace(root.Data, []byte(`"clusterID"`), []byte(`"ClusterID"`), 1)},
		"counter":                {Key: RootKey, Data: bytes.Replace(root.Data, []byte(`"generation":"2"`), []byte(`"generation":"02"`), 1)},
		"unknown":                {Key: RootKey, Data: bytes.Replace(root.Data, []byte(`"formatVersion":1`), []byte(`"formatVersion":1,"other":false`), 1)},
		"surrogate":              {Key: RootKey, Data: bytes.Replace(root.Data, []byte(`"cluster-a"`), []byte(`"cluster-\ud800"`), 1)},
		"invalid utf8":           {Key: RootKey, Data: bytes.Replace(root.Data, []byte(`"cluster-a"`), []byte{'"', 255, '"'}, 1)},
		"missing chunk version":  {Key: chunk.Key, Data: bytes.Replace(chunk.Data, []byte(`,"formatVersion":1`), nil, 1)},
		"null data":              {Key: chunk.Key, Data: json.RawMessage(`{"formatVersion":1,"digest":"` + strings.TrimPrefix(chunk.Key, ChunkPrefix) + `","data":null}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEntry(Section, entry); err == nil {
				t.Fatal("historical ambiguity accepted")
			}
		})
	}
	// Key order/whitespace is irrelevant to exact root CAS.
	var value map[string]any
	if err := json.Unmarshal(root.Data, &value); err != nil {
		t.Fatal(err)
	}
	spaced, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	a, err := Decode(rows, "cluster-a", testAt)
	if err != nil {
		t.Fatal(err)
	}
	spacedRows := copyRows(rows)
	spacedRows[len(spacedRows)-1].Data = spaced
	b, err := Decode(spacedRows, "cluster-a", testAt)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != b.Version {
		t.Fatal("CAS depended on whitespace")
	}
}

func TestHistoricalStateStrictBeforeRestore(t *testing.T) {
	s := fixtureState(t)
	original, err := inventory.EncodeState(s)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"null live":      bytes.Replace(original, []byte(`"live":true`), []byte(`"live":null`), 1),
		"missing live":   bytes.Replace(original, []byte(`,"live":true`), nil, 1),
		"duplicate live": bytes.Replace(original, []byte(`"live":true`), []byte(`"live":true,"live":false`), 1),
		"missing arrays": []byte(`{"formatVersion":1,"generation":"2"}`),
		"case id":        bytes.Replace(original, []byte(`"id"`), []byte(`"ID"`), 1),
		"null source":    bytes.Replace(original, []byte(`"source":"source/uid-a"`), []byte(`"source":null`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			// Recompute every digest: rejection must be the original codec, not hash checking.
			row, err := encodeRow(ChunkPrefix+digest(data), Chunk{FormatVersion: 1, Digest: digest(data), Data: data})
			if err != nil {
				t.Fatal(err)
			}
			root := Root{FormatVersion: 1, ClusterID: "cluster-a", Generation: s.Generation, UpdatedAt: testAt, StateDigest: digest(data), StateBytes: len(data), Chunks: []ChunkRef{{Digest: digest(data), Bytes: len(data)}}}
			r, err := encodeRow(RootKey, root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode([]replication.Entry{row, r}, "cluster-a", testAt); err == nil {
				t.Fatal("invalid original state repaired")
			}
		})
	}
}

func TestBudgetsAndMultiChunkEncoding(t *testing.T) {
	h := inventory.New(func() time.Time { return testAt })
	items := make([]inventory.Client, 1200)
	for i := range items {
		items[i] = client(strings.Repeat("a", 180)+strconv.Itoa(i), "192.0.2.1", testAt)
		items[i].Hostname = "device.test"
	}
	if err := h.Replace("large", 1, items); err != nil {
		t.Fatal(err)
	}
	s, err := h.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	rows := encoded(t, s)
	root := rootOf(t, rows)
	if len(root.Chunks) < 2 {
		t.Fatal("fixture not chunked")
	}
	for _, row := range rows {
		if len(row.Data) > MaxRowBytes {
			t.Fatal("row exceeds envelope cap")
		}
	}
	got, err := Decode(rows, "cluster-a", testAt)
	if err != nil {
		t.Fatal(err)
	}
	if !sameState(t, s, got.State) {
		t.Fatal("multi-chunk evidence changed")
	}
	bad := copyRows(rows)
	bad[0].Data = make([]byte, MaxRowBytes+1)
	if _, err := Decode(bad, "cluster-a", testAt); !errors.Is(err, ErrTooLarge) {
		t.Fatal("row cap not enforced", err)
	}
	bad = make([]replication.Entry, MaxRows+1)
	for i := range bad {
		bad[i] = replication.Entry{Key: Prefix + strconv.Itoa(i), Data: json.RawMessage(`{}`)}
	}
	if _, err := Decode(bad, "cluster-a", testAt); !errors.Is(err, ErrTooLarge) {
		t.Fatal("owner row cap not enforced", err)
	}
	// Large aggregate shares tiny backing strings; preflight rejects before
	// encoding an ~80MiB payload or attempting arbitration allocations.
	tags := make([]string, 64)
	for i := range tags {
		tags[i] = strings.Repeat("x", 128)
	}
	large := inventory.State{FormatVersion: 1, Generation: 1, Sources: []inventory.SourceState{{ID: "a", Revision: 1, Clients: make([]inventory.Client, 10000)}}}
	for i := range large.Sources[0].Clients {
		large.Sources[0].Clients[i] = inventory.Client{Source: "a", ID: "b", Tags: tags}
	}
	if _, err := Encode(large, "cluster-a", testAt); !errors.Is(err, ErrTooLarge) {
		t.Fatal("raw aggregate cap not enforced before restore", err)
	}
}

func sameState(t *testing.T, a, b inventory.State) bool {
	t.Helper()
	x, err := inventory.EncodeState(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := inventory.EncodeState(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

func TestOwnerTotalEncodedBudgetIncludesKeys(t *testing.T) {
	payload := make([]byte, MaxRowBytes)
	rows := make([]replication.Entry, MaxRows)
	for i := range rows {
		rows[i] = replication.Entry{Key: Prefix + strconv.Itoa(i), Data: payload}
	}
	if _, err := Decode(rows, "cluster-a", testAt); !errors.Is(err, ErrTooLarge) {
		t.Fatal("total owner allocation cap ignored", err)
	}
}
func TestEncodeDoesNotRepairNormalizedClientEvidence(t *testing.T) {
	original, err := inventory.EncodeState(fixtureState(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*inventory.Client){
		"trimmed tag":      func(c *inventory.Client) { c.Tags = []string{" tag "} },
		"noncanonical mac": func(c *inventory.Client) { c.MACs = []string{"02:AB:CD:EF:00:01"} },
		"mapped ip":        func(c *inventory.Client) { c.IPs[0] = netip.MustParseAddr("::ffff:192.0.2.1") },
	} {
		t.Run(name, func(t *testing.T) {
			state, err := inventory.DecodeState(original)
			if err != nil {
				t.Fatal(err)
			}
			change(&state.Sources[0].Clients[0])
			if _, err := Encode(state, "cluster-a", testAt); err == nil {
				t.Fatal("authority silently normalized")
			}
		})
	}
}
