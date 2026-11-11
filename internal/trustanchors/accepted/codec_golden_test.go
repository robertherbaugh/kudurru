package accepted

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// The owner rows under testdata were written once (November 11, 2026) by the
// format-1 envelope encoder: a dormant root, and managed roots whose single
// chunk carries the pure format-1 and format-2 golden documents of the parent
// package byte for byte. They are compatibility evidence: never regenerate
// them. A change to their bytes or versions needs a new envelope format and a
// store schema or required-feature bump (D370).
var ownerGoldens = []struct {
	file    string
	phase   Phase
	version Version
}{
	{"owner-dormant.json", Dormant, Version{Generation: 1, Digest: "b139aab2025eb9f9a88d877bf039e4b6e7dba7cda1b32cf6ebad9a2695a6e641"}},
	{"owner-managed-format1.json", Managed, Version{Generation: 3, Digest: "171ee1c1c706fdc6bd70f6a6bbf7f501c838b0c8ba00be0a59e2ba9045cadb8c", Pure: PureVersion{Generation: 2, Digest: "e4f7de991ad94e228f0c40f1ba90c61d01235f4c48e60bc276b9b0ae79f65e36"}}},
	{"owner-managed-format2.json", Managed, Version{Generation: 4, Digest: "905ca0ea24e403b16cdb61da2dfb67342c953a1d4a322a7aee407eae837ddae6", Pure: PureVersion{Generation: 3, Digest: "e1e06f5665d0827324ee92b7b547e6c5a22e78f05f22bb5c0557350daeec5388"}}},
}

func readOwnerGolden(t *testing.T, name string) []replication.Entry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: a golden file under testdata
	if err != nil {
		t.Fatal(err)
	}
	var file []struct {
		Key  string `json:"key"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	rows := make([]replication.Entry, 0, len(file))
	for _, r := range file {
		rows = append(rows, replication.Entry{Key: r.Key, Data: json.RawMessage(r.Data)})
	}
	return rows
}

func TestOwnerGoldenRowsAreByteStable(t *testing.T) {
	for _, g := range ownerGoldens {
		t.Run(g.file, func(t *testing.T) {
			rows := readOwnerGolden(t, g.file)
			s, err := DecodeRows(rows, "cluster")
			if err != nil {
				t.Fatal("golden owner rows no longer decode", err)
			}
			if s.Version() != g.version || s.Inspect().Phase != g.phase || s.Inspect().FormatVersion != FormatVersion {
				t.Fatalf("golden owner version changed: %+v", s.Version())
			}
			encoded, err := EncodeRows(s)
			if err != nil || len(encoded) != len(rows) {
				t.Fatal("re-encoding changed the row set", err)
			}
			for i := range rows {
				if encoded[i].Key != rows[i].Key || !bytes.Equal(encoded[i].Data, rows[i].Data) {
					t.Fatal("decode then encode changed golden row bytes", rows[i].Key)
				}
			}
			image := &replication.Image{Term: replication.Term{ClusterID: "cluster"}, CreatedAt: s.Inspect().UpdatedAt, Payload: replication.Payload{Runtime: rows}}
			if err := ValidateImage(image); err != nil {
				t.Fatal("golden owner rows fail image validation", err)
			}
			if g.phase == Managed {
				state, err := s.State()
				if err != nil {
					t.Fatal(err)
				}
				if v, err := state.Version(); err != nil || v.Digest != g.version.Pure.Digest {
					t.Fatal("managed golden no longer carries the exact pure golden", err)
				}
			}
		})
	}
	// The format-2 envelope chains to the format-1 one it followed.
	two, err := DecodeRows(readOwnerGolden(t, "owner-managed-format2.json"), "cluster")
	if err != nil || two.Inspect().Previous != ownerGoldens[1].version {
		t.Fatal("format-2 envelope lost its previous version", err)
	}
}

// An unknown envelope or chunk format is refused at decode and at entry
// validation, which is what replica image validation runs.
func TestOwnerRejectsUnknownFormat(t *testing.T) {
	for _, file := range []string{"owner-dormant.json", "owner-managed-format2.json"} {
		for _, target := range []string{RootKey, ChunkPrefix} {
			if file == "owner-dormant.json" && target == ChunkPrefix {
				continue
			}
			t.Run(file+"/"+target, func(t *testing.T) {
				rows := readOwnerGolden(t, file)
				changed := false
				for i := range rows {
					if strings.HasPrefix(rows[i].Key, target) {
						next := bytes.Replace(rows[i].Data, []byte(`"formatVersion":1`), []byte(`"formatVersion":2`), 1)
						changed = changed || !bytes.Equal(next, rows[i].Data)
						rows[i].Data = next
						if err := ValidateEntry(Section, rows[i]); err == nil {
							t.Fatal("entry validation accepted an unknown owner format")
						}
					}
				}
				if !changed {
					t.Fatal("fixture mutation did not apply")
				}
				if _, err := DecodeRows(rows, "cluster"); err == nil {
					t.Fatal("unknown owner format decoded")
				}
			})
		}
	}
}
