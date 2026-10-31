package runtimeowners

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	integration "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func openOwnerStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var kinds []store.Kind
	for _, kind := range runtimekinds.Private() {
		kinds = append(kinds, store.Kind(kind))
	}
	st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: kinds, ValidateReplicaEntry: ValidateEntry, ValidateReplicaImage: ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestInitializeAllOwnersPreservesOriginalInventoryOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st := openOwnerStore(t, path)
	at := time.Now().UTC().Add(-time.Minute)
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return Initialize(ctx, tx, at) }); err != nil {
		t.Fatal(err)
	}
	first, err := st.ReadGeneration(t.Context())
	if err != nil || ValidateImage(first) != nil {
		t.Fatal("explicit owners missing", err)
	}
	var want persistence.Version
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		state, err := persistence.Load(ctx, tx, tx.Authority().ClusterID, at)
		if err != nil {
			return err
		}
		ip := netip.MustParseAddr("192.0.2.8")
		client := inventory.Client{Source: "router", ID: "client", IPs: []netip.Addr{ip}, ObservedAt: at, ExpiresAt: at.Add(30 * time.Second)}
		next, err := inventory.PrepareReplacement(state.State, "router", 1, []inventory.Client{client}, at)
		if err != nil {
			return err
		}
		staged, err := persistence.Stage(ctx, tx, state.Version, next, at)
		if err == nil {
			want = staged.Version
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = openOwnerStore(t, path)
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return Initialize(ctx, tx, time.Now().UTC()) }); err != nil {
		t.Fatal(err)
	}
	after, err := st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("startup changed original inventory", err)
	}
	restored, err := persistence.Decode(after.Payload.Runtime, after.Term.ClusterID, after.CreatedAt)
	if err != nil || restored.Version != want || !restored.UpdatedAt.Equal(at) {
		t.Fatal("original version/time lost", err)
	}
	holder, err := inventory.RestoreState(restored.State, time.Now)
	if err != nil || holder.Snapshot().Lookup(netip.MustParseAddr("192.0.2.8"), time.Now()) != nil {
		t.Fatal("expired client revived on restart", err)
	}
}

func TestMissingInventoryRootNeverReconstructsFromSourceAuthority(t *testing.T) {
	for _, prior := range []string{"head", "chunk", "allocation"} {
		t.Run(prior, func(t *testing.T) {
			st := openOwnerStore(t, filepath.Join(t.TempDir(), "state.db"))
			at := time.Now().UTC()
			owner := integration.Owner{ClusterID: st.Info().ClusterID, Namespace: "default", Name: "router", UID: "source-uid"}
			data := []byte(`{}`)
			digest := integration.Digest(data)
			var object store.Object
			switch prior {
			case "chunk":
				object = &integration.IntegrationSourceChunk{ObjectMeta: metav1.ObjectMeta{Name: integration.SourceChunkName(owner, 1, 0, digest)}, Spec: integration.IntegrationSourceChunkSpec{FormatVersion: 1, Owner: owner, Sequence: 1, Digest: digest, Data: data}}
			case "head":
				object = &integration.IntegrationSourceHead{ObjectMeta: metav1.ObjectMeta{Name: integration.SourceHeadName(owner)}, Spec: integration.IntegrationSourceHeadSpec{FormatVersion: 1, Owner: owner, Sequence: 1, ConfigDigest: digest, CredentialDigest: digest, RootDigest: digest, AcceptedAt: at, State: "active", Chunks: []integration.SourceChunkRef{{Name: integration.SourceChunkName(owner, 1, 0, digest), Digest: digest, Bytes: len(data)}}}}
			case "allocation":
				scope := integration.AllocationScope{ClusterID: owner.ClusterID, ZoneUID: "zone-uid", Apex: "example."}
				object = &integration.IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: integration.NameAllocationName(scope, "client")}, Spec: integration.IntegrationNameAllocationSpec{FormatVersion: 1, Scope: scope, Label: "client", Base: "client", ClientID: "client", Owner: owner, CreatedAt: at}}
			}
			if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			before, err := st.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return Initialize(ctx, tx, at) })
			if !errors.Is(err, persistence.ErrUninitialized) {
				t.Fatal("missing inventory adopted existing source authority", err)
			}
			after, err := st.ReadGeneration(t.Context())
			if err != nil || !after.Generation().Same(before.Generation()) || len(after.Payload.Security) != 0 || len(after.Payload.Runtime) != 0 {
				t.Fatal("failure did not roll back every owner", err)
			}
		})
	}
}

func TestCompleteCandidateRejectsDeletedInventoryWithoutResettingIt(t *testing.T) {
	st := openOwnerStore(t, filepath.Join(t.TempDir(), "state.db"))
	at := time.Now().UTC()
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return Initialize(ctx, tx, at) }); err != nil {
		t.Fatal(err)
	}
	image, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stripped := *image
	stripped.Payload = image.Payload
	stripped.Payload.Runtime = []replication.Entry{}
	if err := security.ValidateImage(&stripped); err != nil {
		t.Fatal("test removed other owner", err)
	}
	if err := ValidateImage(&stripped); !errors.Is(err, persistence.ErrUninitialized) {
		t.Fatal("complete image omitted inventory", err)
	}
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.DeleteSection(ctx, persistence.Section, persistence.RootKey)
	}); err != nil {
		t.Fatal(err)
	}
	before, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReadServingCandidate(t.Context()); err == nil {
		t.Fatal("incomplete image got publication token")
	}
	err = st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return Initialize(ctx, tx, at) })
	if !errors.Is(err, persistence.ErrInitialized) {
		t.Fatal("startup repaired orphaned inventory", err)
	}
	after, err := st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("rejected reset changed state", err)
	}
}
