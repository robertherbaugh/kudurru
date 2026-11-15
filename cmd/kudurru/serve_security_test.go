package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func TestOpenStoreInitializesOwnersOnceAndValidatesRows(t *testing.T) {
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Store.Driver = "sqlite"
	cfg.Node.DataDir = t.TempDir()
	cfg.Cache.MaxEntries = 17
	cfg.EnvLocked = []string{"cache.maxEntries"}
	cfg.Blocking.Enabled = false
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	open := func() (*sqlite.Store, func()) {
		t.Helper()
		st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		closeStore := func() { _ = local.Close(); _ = st.Close() }
		t.Cleanup(closeStore)
		return st.(*sqlite.Store), closeStore
	}
	st, closeStore := open()
	first, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := servingauth.DecodeSecurity(first.Payload.Security)
	if err != nil || owner.Epoch != 1 {
		t.Fatal("production driver did not seed owner manifest", err)
	}
	registry, err := providerregistry.Decode(first.Payload.Security, first.Term.ClusterID)
	if err != nil || registry.Epoch() != 1 || len(registry.Sources()) != 0 {
		t.Fatal("production driver did not seed explicit provider registry", err)
	}
	initialInventory, err := persistence.Decode(first.Payload.Runtime, first.Term.ClusterID, first.CreatedAt)
	if err != nil || initialInventory.Version.Generation != 0 || len(initialInventory.State.Sources) != 0 {
		t.Fatal("production driver did not seed explicit inventory", err)
	}
	if _, _, err := st.ReadServingCandidate(t.Context()); err != nil {
		t.Fatal("initialized owners not eligible for preparation", err)
	}
	var settings public.Kudurru
	if err := st.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
		t.Fatal("startup did not seed the canonical singleton", err)
	}
	if len(settings.Spec.Settings) == 0 || len(settings.Spec.Settings["credentials"].Raw) != 0 {
		t.Fatal("public settings missing or host credentials exported")
	}
	var cacheSettings struct{ MaxEntries int }
	var blockingSettings struct{ Enabled bool }
	if err := json.Unmarshal(settings.Spec.Settings["cache"].Raw, &cacheSettings); err != nil || cacheSettings.MaxEntries != 200000 {
		t.Fatal("environment lock leaked into shared settings", err, cacheSettings.MaxEntries)
	}
	if err := json.Unmarshal(settings.Spec.Settings["blocking"].Raw, &blockingSettings); err != nil || blockingSettings.Enabled {
		t.Fatal("original public cluster policy was not migrated", err)
	}
	when := time.Now().UTC()
	err = st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		state, err := security.Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := state.InvalidateGrant("viewer", when); err != nil {
			return err
		}
		return state.Commit(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.ReadGeneration(t.Context())
	closeStore()
	cfg.Cache.MaxEntries++ // a restart must not reseed from newer host defaults
	st, _ = open()
	after, _ := st.ReadGeneration(t.Context())
	owner, err = servingauth.DecodeSecurity(after.Payload.Security)
	if err != nil || !after.Generation().Same(before.Generation()) || owner.Epoch != 2 || len(owner.GrantFloors) != 1 || !owner.GrantFloors[0].ValidAfter.Equal(when) {
		t.Fatal("restart reseeded or changed persisted security history", err)
	}
	err = st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "security", replication.Entry{Key: "auth/v999/root", Data: json.RawMessage(`{}`)})
	})
	if err == nil {
		t.Fatal("production driver omitted owner row validation")
	}
	after, _ = st.ReadGeneration(t.Context())
	if !after.Generation().Same(before.Generation()) {
		t.Fatal("malformed owner row changed complete image")
	}
}

func TestOpenStoreFollowerDoesNotInitializeMissingOwners(t *testing.T) {
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Store.Driver = "sqlite"
	cfg.Node.DataDir = t.TempDir()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sqlite.Open(filepath.Join(cfg.Node.DataDir, sqlite.FileName), sqlite.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower})
	if err != nil {
		t.Fatal(err)
	}
	before := raw.Authority()
	if before.Writable() {
		t.Fatal("test follower is writable")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	opened, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close(); _ = opened.Close() }()
	st := opened.(*sqlite.Store)
	if !reflect.DeepEqual(before, st.Authority()) {
		t.Fatal("follower startup changed persisted authority")
	}
	if _, _, err := st.ReadServingCandidate(t.Context()); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower invented an initial complete image", err)
	}
}
