package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func installImage(t *testing.T, s *sqlite.Store, image *replication.Image) {
	t.Helper()
	v, err := s.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Authority()
	if _, err := s.InstallReplica(t.Context(), v, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed}); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteFollowerRestartPromotionPreservesGlobalRemovalAndExpiry(t *testing.T) {
	primaryPath := filepath.Join(t.TempDir(), sqlite.FileName)
	p := openStore(t, primaryPath, replication.Standalone)
	current := initialize(t, p)
	current = stage(t, p, current, "source/old-uid", 1, []inventory.Client{client("older", "192.0.2.1", testAt), client("expiry", "192.0.2.2", testAt)}, testAt)
	current = stage(t, p, current, "source/newer-uid", 1, []inventory.Client{client("newer", "192.0.2.1", testAt.Add(time.Minute))}, testAt.Add(time.Minute))
	if _, err := p.EnablePrimary(t.Context(), imageOf(t, p).Generation()); err != nil {
		t.Fatal(err)
	}
	followerPath := filepath.Join(t.TempDir(), sqlite.FileName)
	f := openStore(t, followerPath, replication.Follower)
	if err := f.Enroll(t.Context(), p.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installImage(t, f, imageOf(t, p))
	current = stage(t, p, current, "source/newer-uid", 2, nil, testAt.Add(2*time.Minute))
	installImage(t, f, imageOf(t, p))
	want := current.Version
	// Both disk restarts preserve complete evidence, independent of live polls.
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p = openStore(t, primaryPath, replication.Standalone)
	if load(t, p, testAt.Add(72*time.Hour)).Version != want {
		t.Fatal("primary restart changed inventory version")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f = openStore(t, followerPath, replication.Follower)
	image := imageOf(t, f)
	decoded, err := Decode(image.Payload.Runtime, image.Term.ClusterID, image.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Version != want || !sameState(t, decoded.State, current.State) {
		t.Fatal("replica restart changed original evidence")
	}
	before := f.Authority()
	if err := f.Command(t.Context(), before.Term, func(context.Context, *sqlite.CommandTx) error { return nil }); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower admitted writes", err)
	}
	if _, err := f.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: image.Generation(), RequestID: "inventory-promote"}); err != nil {
		t.Fatal(err)
	}
	after := load(t, f, testAt.Add(72*time.Hour))
	if after.Version != want || !sameState(t, after.State, current.State) {
		t.Fatal("promotion renewed/pruned/adopted observations")
	}
	h, err := inventory.RestoreState(after.State, func() time.Time { return testAt.Add(72 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(netip.MustParseAddr("192.0.2.1"), testAt.Add(3*time.Minute)) != nil {
		t.Fatal("promotion resurrected older assignment behind removal")
	}
	if h.Snapshot().Lookup(netip.MustParseAddr("192.0.2.2"), testAt.Add(time.Hour)) != nil {
		t.Fatal("exact expiry boundary renewed on promotion")
	}
	// Recreated source UID cannot inherit a removed address from old evidence.
	after = stage(t, f, after, "source/recreated-uid", 1, []inventory.Client{client("recreated", "192.0.2.1", testAt)}, testAt.Add(3*time.Minute))
	h, err = inventory.RestoreState(after.State, func() time.Time { return testAt.Add(72 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(netip.MustParseAddr("192.0.2.1"), testAt.Add(3*time.Minute)) != nil {
		t.Fatal("new UID adopted old removal authority")
	}
	// The original source revisions survive promotion and reject source replay.
	if _, err := inventory.PrepareReplacement(after.State, "source/newer-uid", 2, nil, testAt.Add(3*time.Minute)); err == nil {
		t.Fatal("source revision floor lost")
	}
}

func TestSQLiteRejectsHistoricalStateAtInstallServingAndPromotion(t *testing.T) {
	p := newStore(t)
	current := initialize(t, p)
	current = stage(t, p, current, "source", 1, []inventory.Client{client("device", "192.0.2.1", testAt)}, testAt)
	good := imageOf(t, p)
	data, err := inventory.EncodeState(current.State)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"live":true`), []byte(`"live":null`), 1)
	chunk, err := encodeRow(ChunkPrefix+digest(data), Chunk{FormatVersion: 1, Digest: digest(data), Data: data})
	if err != nil {
		t.Fatal(err)
	}
	root := Root{FormatVersion: 1, ClusterID: good.Term.ClusterID, Generation: current.Version.Generation, UpdatedAt: testAt, StateDigest: digest(data), StateBytes: len(data), Chunks: []ChunkRef{{Digest: digest(data), Bytes: len(data)}}}
	rootRow, err := encodeRow(RootKey, root)
	if err != nil {
		t.Fatal(err)
	}
	bad := *good
	bad.Payload.Runtime = []replication.Entry{chunk, rootRow}
	if err := replication.Seal(&bad); err != nil {
		t.Fatal(err)
	}
	// Obtain a generically valid image without this owner, simulating an older
	// binary. Receiver install must rerun its local complete-owner validation.
	generic, err := replication.Validate(bad, replication.Validation{SchemaVersion: bad.StoreSchemaVersion, Kinds: bad.KindManifest})
	if err != nil {
		t.Fatal(err)
	}
	f := openStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Follower)
	if err := f.Enroll(t.Context(), good.Term); err != nil {
		t.Fatal(err)
	}
	before := f.Authority()
	if _, err := f.InstallReplica(t.Context(), generic, replication.InstallOptions{ExpectedTerm: before.Term}); err == nil {
		t.Fatal("generic token bypassed original State checks")
	}
	if !before.Term.Same(f.Authority().Term) || before.Installed != f.Authority().Installed {
		t.Fatal("rejected install changed authority")
	}
	// Historical bad rows already on disk remain inspectable for repair, but
	// can neither acquire serving admission nor become writable on promotion.
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	legacy, err := sqlite.Open(path, sqlite.Options{InitialMode: replication.Follower, Validator: sharedValidator()})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Enroll(t.Context(), good.Term); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.InstallReplica(t.Context(), generic, replication.InstallOptions{ExpectedTerm: legacy.Authority().Term}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	legacy = openStore(t, path, replication.Follower)
	before = legacy.Authority()
	if _, _, err := legacy.ReadServingCandidate(t.Context()); err == nil {
		t.Fatal("malformed historical state admitted serving")
	}
	if _, err := legacy.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: bad.Term, ExpectedInstalled: bad.Generation(), RequestID: "bad-promote"}); err == nil {
		t.Fatal("malformed historical state promoted")
	}
	if !before.Term.Same(legacy.Authority().Term) || before.Installed != legacy.Authority().Installed {
		t.Fatal("rejected promotion changed authority")
	}
}

func TestSQLitePrefixPreflightHistoricalRows(t *testing.T) {
	for _, mode := range []string{"row-size", "row-count"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), sqlite.FileName)
			legacy, err := sqlite.Open(path, sqlite.Options{Validator: sharedValidator()})
			if err != nil {
				t.Fatal(err)
			}
			err = legacy.Command(t.Context(), legacy.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if mode == "row-size" {
					return tx.PutSection(ctx, Section, replication.Entry{Key: RootKey, Data: json.RawMessage(`"` + strings.Repeat("x", MaxRowBytes) + `"`)})
				}
				for i := 0; i < MaxRows+1; i++ {
					if err := tx.PutSection(ctx, Section, replication.Entry{Key: Prefix + strconv.Itoa(i), Data: json.RawMessage(`{}`)}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			s := openStore(t, path, replication.Standalone)
			err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, err := Load(ctx, tx, tx.Authority().ClusterID, testAt)
				return err
			})
			if err == nil {
				t.Fatal("historical allocation bound bypassed")
			}
			if mode == "row-count" && !errors.Is(err, replication.ErrTooLarge) {
				t.Fatal("prefix scalar preflight not used", err)
			}
		})
	}
}
