package tsigkeys

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func mutationPrepared(t *testing.T, st *sqlite.Store, seal *Sealer, baseline *dnsserver.StaticKeyring) *Prepared {
	t.Helper()
	source, err := Read(t.Context(), st, st.Info().ClusterID)
	if err != nil {
		t.Fatal("read bounded mutation source", err)
	}
	prepared, err := source.Prepare(seal, baseline)
	if err != nil {
		t.Fatal("prepare complete mutation source", err)
	}
	return prepared
}

func mutationPlan(t *testing.T, prepared *Prepared, seal *Sealer, input MutationInput) *Mutation {
	t.Helper()
	plan, err := prepared.Plan(seal, input)
	if err != nil {
		t.Fatal("prepare typed mutation intent", err)
	}
	return plan
}

func mutationStage(t *testing.T, st *sqlite.Store, plan *Mutation) Metadata {
	t.Helper()
	var result Metadata
	err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		result, err = plan.Stage(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatal("stage typed original mutation", err)
	}
	return result
}

func TestMutationActualEncryptedLifecycleRestartAndMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st := openSourceStore(t, path, replication.Primary)
	seal := testSealer(t, 0x11, 0)
	baseline, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "file.example.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{0x45}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0x63}, 32)
	prepared := mutationPrepared(t, st, seal, baseline)
	plan := mutationPlan(t, prepared, seal, MutationInput{Operation: Create, KeyName: "STORED.EXAMPLE", Algorithm: "hmac-sha256", Secret: secret})
	created := mutationStage(t, st, plan)
	if created.KeyName != "stored.example." || created.UID == "" || created.ResourceVersion == "" || created.Source != "store" || created.CreatedAt.IsZero() {
		t.Fatal("create lost durable metadata")
	}
	prepared = mutationPrepared(t, st, seal, baseline)
	firstID, ok := prepared.Keys().Identity(created.KeyName)
	if !ok {
		t.Fatal("typed create did not prepare a stored credential")
	}
	metadata, err := prepared.Metadata()
	if err != nil || len(metadata) != 2 || metadata[0].Source != "file" || metadata[1] != created {
		t.Fatal("metadata did not retain exact original bindings", err)
	}
	safe, err := json.Marshal(metadata)
	if err != nil || bytes.Contains(safe, secret) || bytes.Contains(safe, []byte(base64.StdEncoding.EncodeToString(secret))) || bytes.Contains(safe, []byte(plan.row.Spec.Sealed)) {
		t.Fatal("metadata exposed private provisioning input", err)
	}
	redacted, err := json.Marshal(plan)
	if err != nil || string(redacted) != `{"private":true}` {
		t.Fatal("plan implicit serialization exposed private row")
	}

	// Store-backed reads survive restart without re-provisioning or a file
	// secret lookup. The file baseline is still the original explicit input.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = openSourceStore(t, path, replication.Primary)
	prepared = mutationPrepared(t, st, seal, baseline)
	restartedID, ok := prepared.Keys().Identity(created.KeyName)
	if !ok || restartedID != firstID {
		t.Fatal("restart changed stored credential identity")
	}

	// The same bytes are a new credential incarnation only for Replace. Both
	// UID and resourceVersion are required from the original captured metadata.
	replaced := mutationStage(t, st, mutationPlan(t, prepared, seal, MutationInput{Operation: Replace, KeyName: created.KeyName, UID: created.UID, ResourceVersion: created.ResourceVersion, Secret: secret}))
	prepared = mutationPrepared(t, st, seal, baseline)
	secondID, ok := prepared.Keys().Identity(created.KeyName)
	if !ok || secondID.CredentialVersion == firstID.CredentialVersion || replaced.UID != created.UID || replaced.ResourceVersion == created.ResourceVersion {
		t.Fatal("same-byte replacement revived original incarnation")
	}

	// Explicit current/previous encryption rotation can prepare the old row,
	// then reseal under current only, without changing its credential epoch.
	nextSeal := testSealer(t, 0x22, 0x11)
	prepared = mutationPrepared(t, st, nextSeal, baseline)
	resealed := mutationStage(t, st, mutationPlan(t, prepared, nextSeal, MutationInput{Operation: Reseal, KeyName: replaced.KeyName, UID: replaced.UID, ResourceVersion: replaced.ResourceVersion}))
	currentOnly := testSealer(t, 0x22, 0)
	prepared = mutationPrepared(t, st, currentOnly, baseline)
	resealedID, ok := prepared.Keys().Identity(created.KeyName)
	if !ok || resealedID != secondID || resealed.ResourceVersion == replaced.ResourceVersion {
		t.Fatal("reseal changed credential epoch or failed to replace ciphertext")
	}
	if _, err := prepared.source.Prepare(seal, baseline); err == nil {
		t.Fatal("old encryption key opened a resealed current-only row")
	}

	mutationStage(t, st, mutationPlan(t, prepared, currentOnly, MutationInput{Operation: Revoke, KeyName: resealed.KeyName, UID: resealed.UID, ResourceVersion: resealed.ResourceVersion}))
	empty := mutationPrepared(t, st, currentOnly, baseline)
	if _, ok := empty.Keys().Identity(created.KeyName); ok {
		t.Fatal("revoked stored credential remained active")
	}
	recreated := mutationStage(t, st, mutationPlan(t, empty, currentOnly, MutationInput{Operation: Create, KeyName: created.KeyName, Algorithm: dns.HmacSHA256, Secret: secret}))
	prepared = mutationPrepared(t, st, currentOnly, baseline)
	thirdID, ok := prepared.Keys().Identity(created.KeyName)
	if !ok || thirdID.CredentialVersion == firstID.CredentialVersion || thirdID.CredentialVersion == secondID.CredentialVersion || recreated.UID == created.UID {
		t.Fatal("delete/recreate revived old owner/epoch")
	}
}

func TestMutationOriginalPreimageAndValidatedPostimageFences(t *testing.T) {
	for _, mode := range []string{"source lag", "late same kind", "invalid complete postimage", "ignored nil plan", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			st := openSourceStore(t, filepath.Join(t.TempDir(), "state.db"), replication.Primary)
			seal := testSealer(t, 0x11, 0)
			baseline, err := dnsserver.NewStaticKeyring()
			if err != nil {
				t.Fatal(err)
			}
			prepared := mutationPrepared(t, st, seal, baseline)
			plan := mutationPlan(t, prepared, seal, MutationInput{Operation: Create, KeyName: "new.example.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{0x64}, 32)})
			if mode == "source lag" {
				applyMaterial(t, st, seal, testMaterial(t, st.Info().ClusterID), "")
			}
			if mode == "invalid complete postimage" {
				foreign := testSealer(t, 0x55, 0)
				material, _, err := seal.Open(plan.row)
				if err != nil {
					t.Fatal(err)
				}
				plan.row, err = foreign.Seal(material)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "ignored nil plan" {
				plan = nil
			}
			before, err := st.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			auditBefore, err := st.ReadAudit(t.Context(), sqlite.AuditQuery{})
			if err != nil {
				t.Fatal(err)
			}
			err = st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "tsig-mutation/unrelated", Data: json.RawMessage(`{}`)}); err != nil {
					return err
				}
				stageCtx := ctx
				if mode == "cancel" {
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					stageCtx = canceled
				}
				_, stageErr := plan.Stage(stageCtx, tx)
				if mode == "late same kind" {
					if stageErr != nil {
						return stageErr
					}
					row, err := seal.Seal(testMaterial(t, st.Info().ClusterID))
					if err != nil {
						return err
					}
					_, err = tx.Apply(ctx, row, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
					return err
				}
				if stageErr == nil {
					t.Error("invalid original/complete source was accepted")
				}
				return nil // deliberately ignore: failure must still poison the tx
			})
			if err == nil {
				t.Fatal("failed typed mutation committed")
			}
			after, readErr := st.ReadGeneration(t.Context())
			if readErr != nil || !after.Generation().Same(before.Generation()) {
				t.Fatal("typed failure leaked source, unrelated row or revision", readErr)
			}
			auditAfter, readErr := st.ReadAudit(t.Context(), sqlite.AuditQuery{})
			if readErr != nil || len(auditAfter) != len(auditBefore) {
				t.Fatal("typed failure leaked audit", readErr)
			}
			if mode == "late same kind" && !errors.Is(err, replication.ErrCAS) {
				t.Fatal("typed owner failed to arm postimage fence", err)
			}
		})
	}
}

func TestMutationExplicitIntentBoundsAndFileExclusion(t *testing.T) {
	st := openSourceStore(t, filepath.Join(t.TempDir(), "state.db"), replication.Primary)
	seal := testSealer(t, 0x11, 0)
	material := testMaterial(t, st.Info().ClusterID)
	row := applyMaterial(t, st, seal, material, "")
	baseline, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "file.example.", Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{0x65}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	prepared := mutationPrepared(t, st, seal, baseline)
	for _, input := range []MutationInput{
		{Operation: Create, KeyName: "file.example.", Algorithm: dns.HmacSHA256, Secret: []byte{1}},
		{Operation: Create, KeyName: material.owner.KeyName, Algorithm: dns.HmacSHA256, Secret: []byte{1}},
		{Operation: Replace, KeyName: material.owner.KeyName, UID: "wrong", ResourceVersion: row.ResourceVersion, Secret: []byte{1}},
		{Operation: Replace, KeyName: material.owner.KeyName, UID: string(row.UID), ResourceVersion: "wrong", Secret: []byte{1}},
		{Operation: Revoke, KeyName: material.owner.KeyName},
		{Operation: Reseal, KeyName: material.owner.KeyName, UID: string(row.UID), ResourceVersion: row.ResourceVersion, Secret: []byte{1}},
		{Operation: Create, KeyName: "huge.example.", Algorithm: dns.HmacSHA256, Secret: make([]byte, MaxSecretBytes+1)},
		{Operation: Create, KeyName: "empty.example.", Algorithm: dns.HmacSHA256},
	} {
		if _, err := prepared.Plan(seal, input); err == nil {
			t.Fatal("invalid lifecycle intent prepared")
		}
	}
	if _, err := prepared.Plan(testSealer(t, 0x22, 0x11), MutationInput{Operation: Revoke, KeyName: material.owner.KeyName, UID: string(row.UID), ResourceVersion: row.ResourceVersion}); err == nil {
		t.Fatal("successor encryption owner prepared original-source mutation")
	}
}
