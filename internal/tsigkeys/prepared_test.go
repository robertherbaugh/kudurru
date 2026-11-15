package tsigkeys

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestPreparedRingEpochResealAndExactCandidate(t *testing.T) {
	s := openSourceStore(t, filepath.Join(t.TempDir(), "prepared.db"), replication.Primary)
	seal := testSealer(t, 0x11, 0)
	original := testMaterial(t, s.Info().ClusterID)
	record := applyMaterial(t, s, seal, original, "")
	baseline, _ := dnsserver.NewStaticKeyring()
	prepare := func(seal *Sealer) *Prepared {
		t.Helper()
		source, err := Read(t.Context(), s, s.Info().ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		p, err := source.Prepare(seal, baseline)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := prepare(seal)
	id, _ := old.Keys().Identity(original.owner.KeyName)
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Matches(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	// Re-encryption preserves key incarnation, but not the exact storage cut.
	nextSeal := testSealer(t, 0x22, 0x11)
	record = applyMaterial(t, s, nextSeal, original, record.ResourceVersion)
	resealed := prepare(nextSeal)
	same, _ := resealed.Keys().Identity(original.owner.KeyName)
	if id != same || old.SourceIdentity() == resealed.SourceIdentity() || old.EncryptionIdentity() == resealed.EncryptionIdentity() {
		t.Fatal("reseal mixed credential and storage identities")
	}
	image, err = s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Matches(t.Context(), image); !errors.Is(err, ErrSource) {
		t.Fatal("old owner matched successor candidate", err)
	}
	replacement := testMaterial(t, s.Info().ClusterID)
	record = applyMaterial(t, s, nextSeal, replacement, record.ResourceVersion)
	rotated := prepare(nextSeal)
	fresh, _ := rotated.Keys().Identity(original.owner.KeyName)
	if fresh == id || fresh.CredentialVersion == id.CredentialVersion {
		t.Fatal("identical secret bytes revived credential epoch")
	}
	if err := s.Delete(t.Context(), store.KeyOf(record), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	recreated := testMaterial(t, s.Info().ClusterID)
	recreated.owner.UID = "recreated-owner"
	applyMaterial(t, s, nextSeal, recreated, "")
	next := prepare(nextSeal)
	recreatedID, _ := next.Keys().Identity(original.owner.KeyName)
	if recreatedID == fresh || recreatedID == id {
		t.Fatal("recreated owner revived old provider identity")
	}
	duplicate, _ := dnsserver.NewStaticKeyring(original.Key())
	source, err := Read(t.Context(), s, s.Info().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Prepare(nextSeal, duplicate); err == nil {
		t.Fatal("stored key silently shadowed file baseline")
	}
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return old.CheckTx(ctx, tx) }); !errors.Is(err, ErrSource) {
		t.Fatal("lagging owner passed actual command", err)
	}
}
