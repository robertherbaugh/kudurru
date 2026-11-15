package follower

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

func open(t *testing.T, mode replication.Mode) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Identity: "node", InitialMode: mode, Validator: testValidator()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func zone() *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary}}
}

type sourceFunc func(context.Context, string) (Response, error)

func (f sourceFunc) Fetch(c context.Context, e string) (Response, error) { return f(c, e) }

type directSource struct{ p *sqlite.Store }

func (s directSource) Fetch(ctx context.Context, etag string) (Response, error) {
	i, err := s.p.ReadGeneration(ctx)
	if err != nil {
		return Response{}, err
	}
	if i.Generation().ETag() == etag {
		return Response{Term: i.Term, ETag: etag, NotModified: true}, nil
	}
	return Response{Term: i.Term, Image: i, ETag: i.Generation().ETag()}, nil
}

func TestPullDeletesUses304AndDoesNotAcknowledgeInvalidContact(t *testing.T) {
	ctx := context.Background()
	primary := open(t, replication.Primary)
	mirror := open(t, replication.Follower)
	if err := mirror.Enroll(ctx, primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Apply(ctx, zone(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	puller := &Puller{Mirror: mirror, Source: directSource{primary}, Now: func() time.Time { return now }}
	if err := puller.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	first := mirror.Authority()
	now = now.Add(time.Minute)
	if err := puller.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if mirror.Authority().InstallSequence != first.InstallSequence || !puller.Status().LastContact.Equal(now) {
		t.Fatal("304 rewrote image or failed to refresh contact")
	}
	if err := primary.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := puller.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	var back v1alpha1.DNSZone
	if err := mirror.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &back); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("absence did not delete")
	}
	contact := puller.Status().LastContact
	puller.Source = sourceFunc(func(context.Context, string) (Response, error) {
		term := mirror.Authority().Term
		return Response{Term: term, ETag: `"wrong"`, NotModified: true}, nil
	})
	if err := puller.Pull(ctx); !errors.Is(err, replication.ErrInvalid) {
		t.Fatalf("bad 304: %v", err)
	}
	if !puller.Status().LastContact.Equal(contact) {
		t.Fatal("invalid response claimed contact")
	}
}

func TestHigherIncompatibleAuthorityStillFencesAndLastGoodRemains(t *testing.T) {
	ctx := context.Background()
	primary := open(t, replication.Primary)
	mirror := open(t, replication.Follower)
	if err := mirror.Enroll(ctx, primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	puller := &Puller{Mirror: mirror, Source: directSource{primary}}
	if err := puller.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := mirror.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	image := *before
	image.Term.Epoch++
	image.Term.PrimaryID = "successor"
	image.Term.PromotionID = "promotion"
	image.StoreSchemaVersion++
	puller.Source = sourceFunc(func(context.Context, string) (Response, error) { return Response{Term: image.Term, Image: &image}, nil })
	if err := puller.Pull(ctx); !errors.Is(err, replication.ErrIncompatible) {
		t.Fatalf("future image accepted: %v", err)
	}
	if mirror.Authority().Mode != replication.Fenced || mirror.Elector("controller").IsLeader() {
		t.Fatal("new authority did not fence")
	}
	after, err := mirror.ReadGeneration(ctx)
	if err != nil || !after.Generation().Same(before.Generation()) {
		t.Fatal("incompatible snapshot replaced last good")
	}
}

func TestFollowerWritesForwardOrFailWithoutLocalFallback(t *testing.T) {
	ctx := context.Background()
	primary := open(t, replication.Primary)
	mirror := open(t, replication.Follower)
	if err := mirror.Enroll(ctx, primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	facade := New(mirror, nil)
	if _, err := facade.Apply(ctx, zone(), store.ApplyOptions{Manager: store.ManagerAPI}); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatalf("missing primary local fallback: %v", err)
	}
	facade = New(mirror, primary)
	if _, err := facade.Apply(ctx, zone(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	var local v1alpha1.DNSZone
	if err := mirror.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &local); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("forwarding wrote local mirror")
	}
	term := mirror.Authority().Term
	term.Epoch++
	term.PrimaryID = "new-primary"
	term.PromotionID = "new-term"
	if _, err := mirror.ObserveAuthority(ctx, term); err != nil {
		t.Fatal(err)
	}
	if _, err := facade.Apply(ctx, zone(), store.ApplyOptions{Manager: store.ManagerAPI}); !errors.Is(err, replication.ErrFenced) {
		t.Fatalf("fenced facade forwarded: %v", err)
	}
}

func TestPullCancellationStopsWithoutNewCluster(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mirror := open(t, replication.Follower)
	entered := make(chan struct{})
	p := &Puller{Mirror: mirror, Source: sourceFunc(func(ctx context.Context, _ string) (Response, error) {
		close(entered)
		<-ctx.Done()
		return Response{}, ctx.Err()
	})}
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pull loop ignored cancellation")
	}
	if mirror.Authority().Mode != replication.Unenrolled || mirror.Info().ClusterID != "" {
		t.Fatal("failed pull bootstrapped a cluster")
	}
}

func TestFormerControllerTokenCannotBecomeForwardedWriteAfterRejoin(t *testing.T) {
	ctx := context.Background()
	old := open(t, replication.Primary)
	newPrimary := open(t, replication.Follower)
	if err := newPrimary.Enroll(ctx, old.Authority().Term); err != nil {
		t.Fatal(err)
	}
	pull := &Puller{Mirror: newPrimary, Source: directSource{old}}
	if err := pull.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := old.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	base, err := newPrimary.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newPrimary.Promote(ctx, replication.PromoteRequest{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation(), RequestID: "promote"}); err != nil {
		t.Fatal(err)
	}
	pull = &Puller{Mirror: old, Source: directSource{newPrimary}}
	if _, err := old.ObserveAuthority(ctx, newPrimary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	if err := pull.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	facade := New(old, newPrimary)
	oldContext := sqlite.WithWriterToken(ctx, token)
	for _, write := range []func() error{
		func() error {
			_, err := facade.Apply(oldContext, zone(), store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		},
		func() error {
			return facade.Delete(oldContext, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{})
		},
		func() error { return facade.UpdateStatus(oldContext, zone()) },
	} {
		if err := write(); !errors.Is(err, replication.ErrFenced) {
			t.Fatalf("stale controller work forwarded: %v", err)
		}
	}
	if _, err := facade.Apply(ctx, zone(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("ordinary user write not forwarded: %v", err)
	}
}
