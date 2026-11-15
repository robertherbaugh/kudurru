package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	enrollmentowner "github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	inventorypersistence "github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

var upgradeValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

func openUpgradeStore(t *testing.T, path string, mode replication.Mode, complete bool) *sqlite.Store {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var kinds []store.Kind
	for _, kind := range runtimekinds.Private() {
		kinds = append(kinds, store.Kind(kind))
	}
	opts := sqlite.Options{Scheme: scheme, InternalKinds: kinds, Validator: upgradeValidator(), Identity: "anchor-upgrade", InitialMode: mode}
	if complete {
		opts.ValidateReplicaEntry, opts.ValidateReplicaImage = runtimeowners.ValidateEntry, runtimeowners.ValidateImage
	}
	s, err := sqlite.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func upgradeImage(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	i, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func upgradeInitialize(t *testing.T, s *sqlite.Store) {
	t.Helper()
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return runtimeowners.Initialize(ctx, tx, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

// Create only the pre-anchor owners in a LOCAL fixture cut, then encode the
// frozen legacy framing explicitly. No received image is resealed or converted.
// SeedSchema6AnchorDatabase subsequently writes it into physical schema6 via
// the real migrations[:6], with the exact old durable follower installed digest.
func legacyAnchorFixture(t *testing.T) replication.Image {
	t.Helper()
	s := openUpgradeStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Primary, false)
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := security.InitializeOwners(ctx, tx); err != nil {
			return err
		}
		_, err := inventorypersistence.Initialize(ctx, tx, tx.Authority().ClusterID, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	i := *upgradeImage(t, s)
	if !accepted.IsAwaitingOwner(i.Payload.Runtime) {
		t.Fatal("local legacy factory already granted anchor history")
	}
	i.Payload.Runtime = slices.DeleteFunc(slices.Clone(i.Payload.Runtime), func(row replication.Entry) bool { return row.Key == accepted.RootKey })
	// The local factory also contains schema9's non-granting marker. Actual
	// schema6 has no enrollment owner; remove only that exact factory marker
	// before freezing the old fixture and writing it via real migrations[:6].
	if !enrollmentowner.IsAwaiting(i.Payload.Membership) {
		t.Fatal("legacy factory already granted enrollment history")
	}
	i.Payload.Membership = slices.DeleteFunc(slices.Clone(i.Payload.Membership), func(row replication.Entry) bool { return row.Key == enrollmentowner.RootKey })
	i.StoreSchemaVersion = 6
	i.RequiredFeatures = []string{"complete-set-v1", "singleton-settings-v1", "opaque-sections-v1", "provider-registry-v1", "inventory-state-v1", "auth-browser-attempts-v1"}
	var err error
	i.PayloadSHA256, err = replication.Digest(i)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func upgradedAnchorPair(t *testing.T) (*sqlite.Store, *sqlite.Store, replication.Image, *replication.Image) {
	t.Helper()
	old := legacyAnchorFixture(t)
	base := replication.Authority{MemberID: old.Term.PrimaryID, Term: old.Term, Mode: replication.Primary, Incarnation: 3, MaxObservedEpoch: old.Term.Epoch, Installed: old.Generation(), WatchLineage: "legacy-primary-lineage"}
	pPath, fPath := filepath.Join(t.TempDir(), "primary.db"), filepath.Join(t.TempDir(), "follower.db")
	sqlite.SeedSchema6AnchorDatabase(t, pPath, old, base)
	base.MemberID, base.Mode, base.WatchLineage, base.InstallSequence = "legacy-follower", replication.Follower, "legacy-follower-lineage", 1
	sqlite.SeedSchema6AnchorDatabase(t, fPath, old, base)
	p := openUpgradeStore(t, pPath, replication.Primary, true)
	f := openUpgradeStore(t, fPath, replication.Follower, true)
	if !reflect.DeepEqual(base, f.Authority()) {
		t.Fatal("physical follower upgrade rewrote old authority receipt")
	}
	transitional := upgradeImage(t, f)
	if !accepted.IsAwaitingOwner(transitional.Payload.Runtime) || transitional.Revision != old.Revision || transitional.PayloadSHA256 == old.PayloadSHA256 {
		t.Fatal("follower upgrade did not retain distinct unaccepted transitional cut")
	}
	upgradeInitialize(t, p)
	next := upgradeImage(t, p)
	if next.Revision != old.Revision+1 || next.ObjectResourceVersion != old.ObjectResourceVersion || !next.Term.Same(old.Term) {
		t.Fatal("primary upgrade did not use ordinary changed Command")
	}
	return p, f, old, next
}

type anchorUpgradeSource struct {
	image *replication.Image
	want  string
	calls int
}

func (s *anchorUpgradeSource) Fetch(_ context.Context, etag string) (follower.Response, error) {
	s.calls++
	if etag != s.want {
		return follower.Response{}, replication.ErrCAS
	}
	return follower.Response{Term: s.image.Term, Image: s.image, ETag: s.image.Generation().ETag()}, nil
}

func TestTrustAnchorActualSchema6FollowerUsesRecomputedBase(t *testing.T) {
	_, f, old, next := upgradedAnchorPair(t)
	before := upgradeImage(t, f)
	if !f.Authority().Installed.Same(old.Generation()) || before.Generation().Same(f.Authority().Installed) {
		t.Fatal("fixture did not retain historical installed cursor")
	}
	if _, _, err := f.ReadServingCandidate(t.Context()); !errors.Is(err, accepted.ErrInvalid) {
		t.Fatal("transitional follower served", err)
	}
	if _, err := f.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: old.Term, ExpectedInstalled: before.Generation(), RequestID: "uninitialized-owner"}); !errors.Is(err, accepted.ErrInvalid) {
		t.Fatal("sentinel follower promoted", err)
	}
	source := &anchorUpgradeSource{image: next, want: before.Generation().ETag()}
	puller := follower.Puller{Mirror: f, Source: source}
	if err := puller.Pull(t.Context()); err != nil || source.calls != 1 {
		t.Fatal("single original upgraded pull failed", err)
	}
	after := upgradeImage(t, f)
	if !after.Generation().Same(next.Generation()) || !reflect.DeepEqual(after.Payload, next.Payload) {
		t.Fatal("installed follower changed primary accepted owner")
	}
	if f.Authority().InstallSequence != 2 {
		t.Fatal("complete install did not advance original sequence exactly once")
	}
	if _, _, err := f.ReadServingCandidate(t.Context()); err != nil {
		t.Fatal("complete upgraded follower not eligible", err)
	}
}

func TestTrustAnchorUpgradeHistoricalBaseAndOldImagesRefuse(t *testing.T) {
	for _, stage := range []string{"historical-base", "old-image", "foreign-validated", "same-revision"} {
		t.Run(stage, func(t *testing.T) {
			p, f, old, _ := upgradedAnchorPair(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := f.BindAnchorLifecycle(ctx); err != nil {
				t.Fatal(err)
			}
			ctx, err := f.CaptureAnchorInstall(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Acquire this operation's source image only after its receiver cut.
			next, err := p.ReadGeneration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			before, authority := upgradeImage(t, f), f.Authority()
			switch stage {
			case "historical-base":
				validated, e := f.ValidateImage(ctx, *next)
				if e != nil {
					t.Fatal(e)
				}
				_, err = f.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: next.Term, ExpectedInstalled: old.Generation()})
			case "old-image":
				source := &anchorUpgradeSource{image: &old, want: before.Generation().ETag()}
				puller := follower.Puller{Mirror: f, Source: source}
				err = puller.Pull(t.Context())
				if source.calls != 1 {
					t.Fatal("refused legacy image was retried")
				}
			case "foreign-validated":
				// Current schema/features with no owner: generic validation on an
				// unregistered store is not the receiver's accepted-owner proof.
				image := *next
				image.Payload.Runtime = slices.Clone(old.Payload.Runtime)
				image.PayloadSHA256, err = replication.Digest(image)
				if err != nil {
					t.Fatal(err)
				}
				generic := openUpgradeStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Standalone, false)
				validated, e := generic.ValidateImage(ctx, image)
				if e != nil {
					t.Fatal("generic fixture invalid", e)
				}
				_, err = f.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: next.Term, ExpectedInstalled: before.Generation()})
			case "same-revision":
				image := *next
				image.Revision = before.Revision
				validated, e := p.ValidateImage(ctx, image)
				if e != nil {
					t.Fatal(e)
				}
				_, err = f.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: next.Term, ExpectedInstalled: before.Generation()})
			}
			want := replication.ErrCAS
			if stage == "old-image" {
				want = replication.ErrIncompatible
			}
			if stage == "foreign-validated" {
				want = accepted.ErrMissing
			}
			if stage == "same-revision" {
				want = replication.ErrDivergent
			}
			if !errors.Is(err, want) {
				t.Fatal("upgrade widened original refusal", err)
			}
			after := upgradeImage(t, f)
			if !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
				t.Fatal("refused install changed original canonical image")
			}
			if stage == "same-revision" {
				if f.Authority().Mode != replication.Fenced || f.Authority().Divergence == nil {
					t.Fatal("same-revision mismatch lost durable divergence")
				}
			} else if !reflect.DeepEqual(authority, f.Authority()) {
				t.Fatal("refused install changed original authority")
			}
		})
	}
}

func TestTrustAnchorFreshFollowerInstallsOnlyPrimaryHistory(t *testing.T) {
	p := openUpgradeStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Primary, true)
	upgradeInitialize(t, p)
	next := upgradeImage(t, p)
	f := openUpgradeStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Follower, true)
	if _, err := f.ReadGeneration(t.Context()); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("unenrolled mirror manufactured a complete image", err)
	}
	if err := f.Enroll(t.Context(), next.Term); err != nil {
		t.Fatal(err)
	}
	called := false
	err := f.Command(t.Context(), next.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		called = true
		return runtimeowners.Initialize(ctx, tx, time.Now().UTC())
	})
	if !errors.Is(err, replication.ErrReadOnly) || called {
		t.Fatal("follower initialized local history", err)
	}
	source := &anchorUpgradeSource{image: next, want: ""}
	puller := follower.Puller{Mirror: f, Source: source}
	if err := puller.Pull(t.Context()); err != nil || source.calls != 1 {
		t.Fatal("first primary install failed", err)
	}
	if got := upgradeImage(t, f); !got.Generation().Same(next.Generation()) || !reflect.DeepEqual(got.Payload, next.Payload) {
		t.Fatal("first install invented follower history")
	}
}

func TestTrustAnchorIncompatibleHigherTermStillFences(t *testing.T) {
	_, f, old, _ := upgradedAnchorPair(t)
	before := upgradeImage(t, f)
	old.Term.Epoch++
	old.Term.PrimaryID, old.Term.PromotionID = "new-primary", "new-term"
	old.Parent = nil
	var err error
	old.PayloadSHA256, err = replication.Digest(old)
	if err != nil {
		t.Fatal(err)
	}
	source := &anchorUpgradeSource{image: &old, want: before.Generation().ETag()}
	puller := follower.Puller{Mirror: f, Source: source}
	if err := puller.Pull(t.Context()); !errors.Is(err, replication.ErrIncompatible) || source.calls != 1 {
		t.Fatal("incompatible source did not retain failure", err)
	}
	if !f.Authority().Term.Same(old.Term) || f.Authority().Writable() {
		t.Fatal("incompatible image concealed authenticated higher authority")
	}
	if got := upgradeImage(t, f); !got.Generation().Same(before.Generation()) {
		t.Fatal("higher term incompatibility replaced installed data")
	}
}
