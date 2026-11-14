package sqlite

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// testdata/anchor-local-managed.json was written once (November 11, 2026) by
// the format-1 local carrier encoder: a managed carrier whose owner is the
// format-2 owner golden of internal/trustanchors/accepted and whose one
// denial first appeared in the format-1 owner golden. Never regenerate it; a
// change to its bytes needs a new local format (D370).
const anchorLocalGoldenDigest = "bae4b2cc1d6853e8b1d34d8adcd28d588f0dc8b265fb766ddb958c9bcb3307c1"

func readAnchorLocalGolden(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "anchor-local-managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAnchorLocalGoldenRecordIsByteStable(t *testing.T) {
	raw := readAnchorLocalGolden(t)
	local, err := decodeAnchorLocal(raw)
	if err != nil {
		t.Fatal("golden local carrier no longer decodes", err)
	}
	if local.raw != raw || hex.EncodeToString(local.digest[:]) != anchorLocalGoldenDigest {
		t.Fatal("decode then encode changed the golden local carrier", hex.EncodeToString(local.digest[:]))
	}
	again, err := encodeAnchorLocal(local.record)
	if err != nil || again.raw != raw || !sameAnchorLocal(local, again) {
		t.Fatal("re-encoding changed the golden local carrier", err)
	}
	r := local.record
	if r.Format != 1 || r.Phase != "managed" || r.Owner.Generation != 4 || len(r.Denied) != 1 || r.Denied[0].FirstOwner.Generation != 3 {
		t.Fatal("golden local carrier changed meaning")
	}
	for _, format := range []string{`"format":2,`, `"format":0,`, `"format":"1",`} {
		mutated := strings.Replace(raw, `"format":1,`, format, 1)
		if mutated == raw {
			t.Fatal("fixture mutation did not apply")
		}
		if _, err := decodeAnchorLocal(mutated); err == nil {
			t.Fatal("unknown local carrier format decoded", format)
		}
	}
}

// TestAnchorFormatGateTuple is a tripwire. Every exchange of anchor history
// between members is gated by the exact store schema and required-feature
// list, not by the inner formats. A change to any inner format (pure,
// recovery, owner envelope, local carrier) must therefore come with a schema
// or RequiredFeatures bump, and this test must be updated in the same change
// with a decision-log entry (D370).
func TestAnchorFormatGateTuple(t *testing.T) {
	type tuple struct {
		Schema                                      int
		Features                                    []string
		Pure, Recovery, Envelope, LocalCarrier, Row int
	}
	local, err := decodeAnchorLocal(readAnchorLocalGolden(t))
	if err != nil {
		t.Fatal(err)
	}
	got := tuple{Schema: store.SchemaVersion, Features: replication.FeatureNames(), Pure: trustanchors.FormatVersion, Recovery: trustanchors.RecoveryFormatVersion, Envelope: accepted.FormatVersion, LocalCarrier: local.record.Format, Row: replication.FormatVersion}
	want := tuple{Schema: 9, Features: []string{"complete-set-v1", "singleton-settings-v1", "opaque-sections-v1", "provider-registry-v1", "inventory-state-v1", "auth-browser-attempts-v1", "managed-root-anchors-v1", "managed-root-runtime-v1", "member-enrollment-v1"},
		Pure: 1, Recovery: 2, Envelope: 1, LocalCarrier: 1, Row: replication.FormatVersion}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("anchor format gate tuple changed without updating this tripwire and D370:\n got %+v\nwant %+v", got, want)
	}
	if accepted.RootKey != "trustanchors/v1/root" || accepted.ChunkPrefix != "trustanchors/v1/chunks/" || anchorLocalKey != "trustanchors-local/v1/state" || anchorLocalProvenance != "physical-schema-8" {
		t.Fatal("anchor row namespaces changed without a format decision")
	}
}

// anchorReframe replaces the owner chunks of an image with one chunk holding
// pure, and rewrites the root's chunk, digest and size fields to match, so
// only the pure document's own content can cause a refusal.
func anchorReframe(t *testing.T, rows []replication.Entry, pure []byte) []replication.Entry {
	t.Helper()
	sum := sha256.Sum256(pure)
	hash := hex.EncodeToString(sum[:])
	out := slices.DeleteFunc(slices.Clone(rows), func(r replication.Entry) bool { return strings.HasPrefix(r.Key, accepted.ChunkPrefix) })
	chunk, err := json.Marshal(map[string]any{"data": base64.StdEncoding.EncodeToString(pure), "digest": hash, "formatVersion": 1})
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, replication.Entry{Key: accepted.ChunkPrefix + hash, Data: chunk})
	for i := range out {
		if out[i].Key != accepted.RootKey {
			continue
		}
		var root map[string]any
		if err := json.Unmarshal(out[i].Data, &root); err != nil {
			t.Fatal(err)
		}
		root["chunks"] = []map[string]any{{"bytes": len(pure), "digest": hash}}
		root["stateBytes"] = len(pure)
		root["pure"].(map[string]any)["digest"] = hash
		if out[i].Data, err = json.Marshal(root); err != nil {
			t.Fatal(err)
		}
	}
	slices.SortFunc(out, func(a, b replication.Entry) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// An image whose owner envelope, owner chunk or pure document carries an
// unknown format is refused by a current receiver, and the receiver's image,
// authority and local carrier stay exactly as they were. The unmodified image
// then installs, so each refusal is caused by the format alone.
func TestAnchorInstallRefusesUnknownFormatsUnchanged(t *testing.T) {
	primary, follower, _ := anchorTestStaticPair(t)
	anchorTestManaged(t, primary, primary.anchors.Load().cursor.Load().owner)
	managed := imageOf(t, primary)
	current, err := accepted.DecodeRows(managed.Payload.Runtime, managed.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := current.State()
	if err != nil {
		t.Fatal(err)
	}
	pure, err := trustanchors.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func([]replication.Entry) []replication.Entry
	}{
		{"control-reframe", func(rows []replication.Entry) []replication.Entry { return anchorReframe(t, rows, pure) }},
		{"envelope-format-2", func(rows []replication.Entry) []replication.Entry {
			return anchorFormatReplace(t, rows, accepted.RootKey, `"formatVersion":1`, `"formatVersion":2`)
		}},
		{"chunk-format-2", func(rows []replication.Entry) []replication.Entry {
			return anchorFormatReplace(t, rows, accepted.ChunkPrefix, `"formatVersion":1`, `"formatVersion":2`)
		}},
		{"pure-format-3", func(rows []replication.Entry) []replication.Entry {
			next := strings.Replace(string(pure), `{"format":1,`, `{"format":3,`, 1)
			if next == string(pure) {
				t.Fatal("fixture mutation did not apply")
			}
			return anchorReframe(t, rows, []byte(next))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := *managed
			image.Payload.Runtime = tc.mutate(slices.Clone(managed.Payload.Runtime))
			if err := replication.Seal(&image); err != nil {
				t.Fatal(err)
			}
			beforeImage, beforeAuthority, beforeLocal := imageOf(t, follower).Generation(), follower.Authority(), anchorLocalTestRead(t, follower)
			ctx := anchorTestInstallContext(t.Context(), t, follower)
			validated, err := follower.ValidateImage(ctx, image)
			if err == nil && tc.name != "control-reframe" {
				_, err = follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: beforeImage})
			}
			if tc.name == "control-reframe" {
				// The helper alone must not cause a refusal. Do not install it:
				// the later subtests need the original receiver.
				if _, verr := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID); verr != nil || validated == nil {
					t.Fatal("reframing the exact pure document was refused", verr, err)
				}
				return
			}
			if err == nil {
				t.Fatal("receiver installed an unknown anchor format")
			}
			if !beforeImage.Same(imageOf(t, follower).Generation()) || !reflect.DeepEqual(beforeAuthority, follower.Authority()) || !sameAnchorLocal(beforeLocal, anchorLocalTestRead(t, follower)) {
				t.Fatal("refused unknown format changed the receiver")
			}
		})
	}
	ctx := anchorTestInstallContext(t.Context(), t, follower)
	validated, err := follower.ValidateImage(ctx, *managed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: managed.Term, ExpectedInstalled: follower.Authority().Installed}); err != nil {
		t.Fatal("unmodified managed image was refused", err)
	}
	// A durable local carrier in an unknown format is refused when read.
	raw := anchorLocalTestRead(t, follower).raw
	if _, err := follower.w.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key=?`, strings.Replace(raw, `"format":1,`, `"format":2,`, 1), anchorLocalKey); err != nil {
		t.Fatal(err)
	}
	if local, err := loadAnchorLocal(t.Context(), follower.r); err == nil {
		t.Fatal("unknown local carrier format loaded", local.record.Format)
	}
}

func anchorFormatReplace(t *testing.T, rows []replication.Entry, prefix, old, next string) []replication.Entry {
	t.Helper()
	changed := false
	for i := range rows {
		if strings.HasPrefix(rows[i].Key, prefix) {
			data := strings.Replace(string(rows[i].Data), old, next, 1)
			changed = changed || data != string(rows[i].Data)
			rows[i].Data = json.RawMessage(data)
		}
	}
	if !changed {
		t.Fatal("fixture mutation did not apply")
	}
	return rows
}
