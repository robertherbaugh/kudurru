package tsigkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func openSourceStore(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, k := range runtimekinds.Private() {
		private = append(private, store.Kind(k))
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: private, InitialMode: mode})
	if err != nil {
		t.Fatal("open TSIG source fixture", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func applyMaterial(t *testing.T, s *sqlite.Store, seal *Sealer, m *Material, rv string) *kinds.TSIGKey {
	t.Helper()
	k, err := seal.Seal(m)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Apply(t.Context(), k, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: rv == "", ExpectedResourceVersion: rv})
	if err != nil {
		t.Fatal("persist encrypted fixture", err)
	}
	return out.(*kinds.TSIGKey)
}

func TestActualSourceTransactionLagRollbackAndRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSourceStore(t, path, replication.Primary)
	seal := testSealer(t, 0x11, 0)
	m := testMaterial(t, s.Info().ClusterID)
	k := applyMaterial(t, s, seal, m, "")
	cut, err := Read(ctx, s, s.Info().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return cut.CheckTx(ctx, tx) }); err != nil {
		t.Fatal("original source rejected", err)
	}
	// Updating another row cannot alter the detached original carrier or its bytes.
	newMaterial := testMaterial(t, s.Info().ClusterID)
	k = applyMaterial(t, s, seal, newMaterial, k.ResourceVersion)
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_ = tx.PutSection(ctx, "runtime", replication.Entry{Key: "tsig-test/escaped", Data: json.RawMessage(`{}`)})
		_ = cut.CheckTx(ctx, tx) // deliberately ignored: admission must still fail
		return nil
	}); !errors.Is(err, ErrSource) {
		t.Fatal("lagging source authorized or ignored check escaped", err)
	}
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.GetSection(ctx, "runtime", "tsig-test/escaped")
		if !errors.Is(err, store.ErrNotFound) {
			t.Error("failed source check leaked staged state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	materials, _, err := cut.Open(seal)
	if err != nil || len(materials) != 1 || materials[0].epoch != m.epoch {
		t.Fatal("old captured source mutated")
	}
	current, err := Read(ctx, s, s.Info().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Identity() == cut.Identity() {
		t.Fatal("replacement reused original source identity")
	}
	// Resealing the same credential still changes the exact persisted row cut.
	applyMaterial(t, s, seal, newMaterial, k.ResourceVersion)
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return current.CheckTx(ctx, tx) }); !errors.Is(err, ErrSource) {
		t.Fatal("reseal revived earlier row proof", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openSourceStore(t, path, replication.Primary)
	restarted, err := Read(ctx, s, s.Info().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	materials, previous, err := restarted.Open(testSealer(t, 0x22, 0x11))
	if err != nil || !previous || materials[0].epoch != newMaterial.epoch {
		t.Fatal("restart lost original encrypted owner")
	}
	if _, _, err := restarted.Open(testSealer(t, 0x22, 0)); err == nil {
		t.Fatal("missing decrypt key treated as empty source")
	}
	if _, err := Read(ctx, s, "another-cluster"); err == nil {
		t.Fatal("wrong cluster source admitted")
	}
	// Original opaque handles cannot be used beyond the synchronous command.
	var ended *sqlite.CommandTx
	if err := s.Command(ctx, s.Authority().Term, func(_ context.Context, tx *sqlite.CommandTx) error { ended = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTx(ctx, ended); !errors.Is(err, sqlite.ErrTransactionEnded) {
		t.Fatal("ended transaction accepted", err)
	}
}

func TestActualSourceReplicaPromotionAndPrivateBackup(t *testing.T) {
	ctx := t.Context()
	s := openSourceStore(t, filepath.Join(t.TempDir(), "primary.db"), replication.Primary)
	seal := testSealer(t, 0x11, 0)
	m := testMaterial(t, s.Info().ClusterID)
	k := applyMaterial(t, s, seal, m, "")
	var backup bytes.Buffer
	for _, safe := range []bool{true, false} {
		var b bytes.Buffer
		if err := s.Snapshot(ctx, &b, store.SnapshotOptions{Safe: safe}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "kind: TSIGKey") == safe {
			t.Fatal("private backup classification mismatch")
		}
		if bytes.Contains(b.Bytes(), m.Key().Secret) {
			t.Fatal("backup exposed raw key")
		}
		if !safe {
			backup.Write(b.Bytes())
		}
	}
	f := openSourceStore(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err := f.Enroll(ctx, s.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := f.BindAnchorLifecycle(installCtx); err != nil {
		t.Fatal(err)
	}
	installCtx, err := f.CaptureAnchorInstall(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	image, err := s.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := FromImage(installCtx, image)
	if err != nil || cut.Len() != 1 {
		t.Fatal("image source unavailable", err)
	}
	validated, err := f.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: f.Authority().Term}); err != nil {
		t.Fatal(err)
	}
	copied, err := Read(ctx, f, f.Info().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if copied.Identity() != cut.Identity() {
		t.Fatal("replica changed source ownership")
	}
	if _, _, err := copied.Open(seal); err != nil {
		t.Fatal("replica lost encrypted material")
	}
	if _, err := f.Promote(ctx, replication.PromoteRequest{ExpectedTerm: f.Authority().Term, ExpectedInstalled: image.Generation(), RequestID: "tsig-source-promotion"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, store.KeyOf(k), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	if err := f.Restore(ctx, bytes.NewReader(backup.Bytes())); err != nil {
		t.Fatal("restore private encrypted backup", err)
	}
	restored, err := Read(ctx, f, f.Info().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	opened, _, err := restored.Open(seal)
	if err != nil || opened[0].epoch != m.epoch {
		t.Fatal("restore changed credential or owner")
	}
	if restored.Identity() == cut.Identity() {
		t.Fatal("backup restore revived old transaction cut")
	}
	// A missing private manifest cannot stand in for an explicit empty source.
	for i, kind := range image.KindManifest {
		if kind.Kind == kinds.KindTSIGKey {
			image.KindManifest = append(image.KindManifest[:i], image.KindManifest[i+1:]...)
			break
		}
	}
	if _, err := FromImage(ctx, image); err == nil {
		t.Fatal("missing TSIG manifest accepted")
	}
}

func TestActualSourceBoundsPoisonAndOriginalCancellation(t *testing.T) {
	s := openSourceStore(t, filepath.Join(t.TempDir(), "bounded.db"), replication.Primary)
	seal := testSealer(t, 0x11, 0)
	applyMaterial(t, s, seal, testMaterial(t, s.Info().ClusterID), "")
	ctx := t.Context()
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_ = tx.CheckKindBounds(ctx, kinds.KindTSIGKey, 0, 0)
		return nil
	}); !errors.Is(err, replication.ErrTooLarge) {
		t.Fatal("ignored bounds failure was not poisoned", err)
	}
	cut, err := Read(ctx, s, s.Info().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Command(canceled, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return cut.CheckTx(ctx, tx) }); !errors.Is(err, context.Canceled) {
		t.Fatal("original cancellation lost", err)
	}
	// One actual writer holds its original read cut; a concurrent rotation waits.
	entered, release, commandDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		commandDone <- s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if err := cut.CheckTx(ctx, tx); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	replacement, err := seal.Seal(testMaterial(t, s.Info().ClusterID))
	if err != nil {
		t.Fatal(err)
	}
	rotated := make(chan error, 1)
	go func() {
		rows, err := s.List(ctx, kinds.KindTSIGKey, store.ListOptions{})
		if err != nil {
			rotated <- err
			return
		}
		_, err = s.Apply(ctx, replacement, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: rows.Items[0].GetResourceVersion()})
		rotated <- err
	}()
	close(release)
	select {
	case err := <-commandDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("original command did not join")
	}
	select {
	case err := <-rotated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not join")
	}
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return cut.CheckTx(ctx, tx) }); !errors.Is(err, ErrSource) {
		t.Fatal("concurrent replacement adopted by old source", err)
	}
}

// The original transaction sees staged rows, and an oversized inventory cannot
// commit even if a caller ignores its read error after staging unrelated work.
func TestActualStagedSourceCapacityIsAtomic(t *testing.T) {
	s := openSourceStore(t, filepath.Join(t.TempDir(), "capacity.db"), replication.Primary)
	before, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seal := testSealer(t, 0x11, 0)
	err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for i := 0; i <= kinds.MaxKeys; i++ {
			material, err := NewMaterial(Owner{ClusterID: s.Info().ClusterID, UID: "capacity-owner", KeyName: fmt.Sprintf("key-%d.example.", i), Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x75}, 32))
			if err != nil {
				return err
			}
			row, err := seal.Seal(material)
			if err != nil {
				return err
			}
			if _, err := tx.Apply(ctx, row, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); err != nil {
				return err
			}
		}
		_, _ = ReadTx(ctx, tx)
		return nil
	})
	if !errors.Is(err, replication.ErrTooLarge) {
		t.Fatal("oversized staged source admitted", err)
	}
	after, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !after.Generation().Same(before.Generation()) {
		t.Fatal("oversized source leaked staged rows")
	}
}

type sourceReader func(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)

func (r sourceReader) List(ctx context.Context, k store.Kind, o store.ListOptions) (store.ObjectList, error) {
	return r(ctx, k, o)
}

func TestSourceCompletenessAndMetadataRefuseWholeCandidate(t *testing.T) {
	s := openSourceStore(t, filepath.Join(t.TempDir(), "metadata.db"), replication.Primary)
	seal := testSealer(t, 0x11, 0)
	valid := applyMaterial(t, s, seal, testMaterial(t, s.Info().ClusterID), "")
	for _, tc := range []struct {
		name string
		rows func() store.ObjectList
	}{
		{"continuation", func() store.ObjectList { return store.ObjectList{Continue: "next", Items: []store.Object{valid}} }},
		{"overfull", func() store.ObjectList {
			rows := store.ObjectList{}
			for range kinds.MaxKeys + 1 {
				rows.Items = append(rows.Items, valid)
			}
			return rows
		}},
		{"uid", func() store.ObjectList {
			bad := valid.DeepCopy()
			bad.UID = ""
			return store.ObjectList{Items: []store.Object{bad}}
		}},
		{"rv", func() store.ObjectList {
			bad := valid.DeepCopy()
			bad.ResourceVersion = ""
			return store.ObjectList{Items: []store.Object{bad}}
		}},
		{"deleting", func() store.ObjectList {
			bad := valid.DeepCopy()
			now := metav1.Now()
			bad.DeletionTimestamp = &now
			return store.ObjectList{Items: []store.Object{bad}}
		}},
		{"duplicate", func() store.ObjectList { return store.ObjectList{Items: []store.Object{valid, valid.DeepCopy()}} }},
		{"foreign", func() store.ObjectList {
			bad := valid.DeepCopy()
			bad.Spec.ClusterID = "foreign"
			return store.ObjectList{Items: []store.Object{bad}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := sourceReader(func(_ context.Context, k store.Kind, opts store.ListOptions) (store.ObjectList, error) {
				if k != kinds.KindTSIGKey || opts.Limit != kinds.MaxKeys+1 {
					t.Error("source read was not bounded")
				}
				return tc.rows(), nil
			})
			if _, err := Read(t.Context(), reader, s.Info().ClusterID); err == nil {
				t.Fatal("incomplete source accepted")
			}
		})
	}
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cut, err := FromImage(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	// A valid public envelope with undecryptable content is not an empty source.
	cut.records[0].Spec.Sealed = testSealer(t, 0x22, 0).seal.Seal([]byte(`{}`))
	if materials, _, err := cut.Open(seal); err == nil || len(materials) != 0 {
		t.Fatal("unavailable stored key partially prepared")
	}
}
