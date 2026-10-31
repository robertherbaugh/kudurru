package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

var errTestInventory = errors.New("test owner: incomplete inventory")

// Both rows are individually valid JSON. Only the complete owner can enforce
// the root's required child, including explicit empty/missing-root semantics.
func testInventory(image *replication.Image) error {
	rows := image.Payload.Security
	if len(rows) != 2 || rows[0].Key != "owner/item" || rows[1].Key != "owner/root" {
		return errTestInventory
	}
	return nil
}

func seedTestInventory(t *testing.T, s *Store) {
	t.Helper()
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		for _, key := range []string{"owner/root", "owner/item"} {
			if err := tx.PutSection(ctx, "security", replication.Entry{Key: key, Data: json.RawMessage(`{}`)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOwnerInventoryAllowsExplicitMigrationBeforeServingOrHA(t *testing.T) {
	s := openTemp(t, func(o *Options) { o.ValidateReplicaImage = testInventory })
	empty := imageOf(t, s) // Raw migration input remains readable.
	if _, token, err := s.ReadServingCandidate(t.Context()); !errors.Is(err, errTestInventory) || token.Generation().Term.ClusterID != "" {
		t.Fatal("incomplete owner received serving token", err)
	}
	if _, err := s.EnablePrimary(t.Context(), empty.Generation()); !errors.Is(err, errTestInventory) {
		t.Fatal("incomplete owner enabled HA", err)
	}
	seedTestInventory(t, s)
	i, token, err := s.ReadServingCandidate(t.Context())
	if err != nil || !token.Generation().Same(i.Generation()) {
		t.Fatal("explicit migration not admitted", err)
	}
	if _, err := s.EnablePrimary(t.Context(), i.Generation()); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerInventoryRevalidatedAtReplicaInstallAndPromotion(t *testing.T) {
	p := openTemp(t)
	seedTestInventory(t, p)
	good := imageOf(t, p)
	// Generic validation on another store is not proof of this owner's checks.
	bad := *good
	bad.Payload.Security = good.Payload.Security[:1]
	if err := replication.Seal(&bad); err != nil {
		t.Fatal(err)
	}
	generic, err := p.ValidateImage(t.Context(), bad)
	if err != nil {
		t.Fatal(err)
	}
	f := openTemp(t, func(o *Options) {
		o.InitialMode = replication.Follower
		o.ValidateReplicaImage = testInventory
	})
	if err := f.Enroll(t.Context(), p.Authority().Term); err != nil {
		t.Fatal(err)
	}
	before := f.Authority()
	if _, err := f.InstallReplica(t.Context(), generic, replication.InstallOptions{ExpectedTerm: before.Term}); !errors.Is(err, errTestInventory) {
		t.Fatal("foreign validated token bypassed owner", err)
	}
	if !reflect.DeepEqual(before, f.Authority()) {
		t.Fatal("rejected install changed authority")
	}
	install(t, f, good)
	if _, err := f.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: good.Term, ExpectedInstalled: good.Generation(), RequestID: "complete"}); err != nil {
		t.Fatal(err)
	}

	// Reopen a legacy follower under the newly registered owner. Opening and
	// raw inspection still work, but neither promotion nor serving is allowed.
	path := filepath.Join(t.TempDir(), FileName)
	opts := testOptions()
	opts.InitialMode = replication.Follower
	legacy, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Enroll(t.Context(), p.Authority().Term); err != nil {
		t.Fatal(err)
	}
	install(t, legacy, &bad)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	opts.ValidateReplicaImage = testInventory
	legacy, err = Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	before = legacy.Authority()
	if _, err := legacy.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: bad.Term, ExpectedInstalled: bad.Generation(), RequestID: "incomplete"}); !errors.Is(err, errTestInventory) {
		t.Fatal("incomplete legacy image promoted", err)
	}
	if !reflect.DeepEqual(before, legacy.Authority()) {
		t.Fatal("rejected promotion changed authority")
	}
	if _, _, err := legacy.ReadServingCandidate(t.Context()); !errors.Is(err, errTestInventory) {
		t.Fatal("incomplete legacy image served", err)
	}
}
