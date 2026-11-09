package tsigkeys

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// failingBind passes every call through except the second Apply (the bind of
// a two-phase create), which it refuses after the provisional carrier exists.
type failingBind struct {
	StoreWriter
	applies int
}

func (f *failingBind) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	f.applies++
	if f.applies == 2 {
		return nil, errors.New("test: bind refused")
	}
	return f.StoreWriter.Apply(ctx, obj, opts)
}

func TestApplyStoreTwoPhaseCreateCompensationAndUnusableRevoke(t *testing.T) {
	st := openSourceStore(t, filepath.Join(t.TempDir(), "state.db"), replication.Primary)
	seal := testSealer(t, 0x11, 0)
	baseline, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	prepared := mutationPrepared(t, st, seal, baseline)
	create := MutationInput{Operation: Create, KeyName: "two-phase.", Algorithm: "hmac-sha256", Secret: bytes.Repeat([]byte{0x51}, 32)}

	// An interrupted bind removes exactly its provisional carrier.
	failing := &failingBind{StoreWriter: st}
	if _, err := mutationPlan(t, prepared, seal, create).ApplyStore(t.Context(), failing); !errors.Is(err, ErrProvisional) {
		t.Fatal("interrupted bind not reported as provisional", err)
	}
	after := mutationPrepared(t, st, seal, baseline)
	if after.SourceIdentity() != prepared.SourceIdentity() {
		t.Fatal("failed create left a carrier behind")
	}

	// A complete create binds the assigned UID and prepares.
	metadata, err := mutationPlan(t, after, seal, create).ApplyStore(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	created := mutationPrepared(t, st, seal, baseline)
	if _, algorithm, ok := created.Keys().Lookup("two-phase."); !ok || algorithm != dns.HmacSHA256 || metadata.UID == "" {
		t.Fatal("created key absent from the prepared ring", metadata)
	}
	// A plan bound to a superseded source refuses.
	stale := mutationPlan(t, after, seal, MutationInput{Operation: Create, KeyName: "other.", Algorithm: "hmac-sha256", Secret: bytes.Repeat([]byte{0x52}, 32)})
	if _, err := stale.ApplyStore(t.Context(), st); !errors.Is(err, ErrPrecondition) {
		t.Fatal("stale source applied", err)
	}
	// A carrier that opens cannot be removed through the unusable path.
	if _, err := RevokeUnusable(t.Context(), st, seal, "two-phase.", metadata.UID, metadata.ResourceVersion); !errors.Is(err, ErrPrecondition) {
		t.Fatal("usable carrier removed through the unusable path", err)
	}
	// An unusable carrier (sealed under another key) is removable exactly.
	other := testSealer(t, 0x77, 0)
	foreign, err := NewMaterial(Owner{ClusterID: created.ClusterID(), UID: "foreign-owner", KeyName: "foreign.", Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x53}, 32))
	if err != nil {
		t.Fatal(err)
	}
	row, err := other.Seal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := st.Apply(t.Context(), row, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(t.Context(), st, created.ClusterID()); err != nil {
		t.Fatal(err)
	}
	if _, err := RevokeUnusable(t.Context(), st, seal, "foreign.", string(obj.GetUID()), "0"); !errors.Is(err, ErrPrecondition) {
		t.Fatal("unusable revoke ignored a stale version", err)
	}
	if _, err := RevokeUnusable(t.Context(), st, seal, "foreign.", string(obj.GetUID()), obj.GetResourceVersion()); err != nil {
		t.Fatal(err)
	}
	if mutationPrepared(t, st, seal, baseline).SourceIdentity() != created.SourceIdentity() {
		t.Fatal("unusable revoke changed other carriers")
	}
}
