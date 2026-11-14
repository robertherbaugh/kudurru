package follower

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestPullRetainsOriginalAnchorCutBeforeFetchAnd304(t *testing.T) {
	for _, notModified := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-image", true: "retained-304"}[notModified], func(t *testing.T) {
			primary := open(t, replication.Primary)
			mirror := open(t, replication.Follower)
			if err := mirror.Enroll(t.Context(), primary.Authority().Term); err != nil {
				t.Fatal(err)
			}
			puller := &Puller{Mirror: mirror, Source: directSource{primary}}
			if notModified {
				if err := puller.Pull(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			contact := puller.Status().LastContact
			calls := 0
			puller.Source = sourceFunc(func(ctx context.Context, etag string) (Response, error) {
				calls++
				// This independent source event installs after the original
				// capture. The held operation cannot adopt its accepted cut.
				if _, err := primary.Apply(ctx, zone(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					return Response{}, err
				}
				winner := &Puller{Mirror: mirror, Source: directSource{primary}}
				if err := winner.Pull(t.Context()); err != nil {
					return Response{}, err
				}
				if notModified {
					return Response{Term: primary.Authority().Term, NotModified: true, ETag: etag}, nil
				}
				return (directSource{primary}).Fetch(ctx, "")
			})
			if err := puller.Pull(t.Context()); !errors.Is(err, replication.ErrCAS) {
				t.Fatal("held acquisition adopted an interposed installed cut", err)
			}
			if calls != 1 || !puller.Status().LastContact.Equal(contact) {
				t.Fatal("refused acquisition retried or acknowledged contact")
			}
			got, err := mirror.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want, err := primary.ReadGeneration(t.Context())
			if err != nil || !got.Generation().Same(want.Generation()) {
				t.Fatal("held refusal replaced independently installed image", err)
			}
		})
	}
}

func TestPullHigherAuthorityFencesWithoutRecapturingOriginal(t *testing.T) {
	primary := open(t, replication.Primary)
	mirror := open(t, replication.Follower)
	if err := mirror.Enroll(t.Context(), primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	puller := &Puller{Mirror: mirror, Source: directSource{primary}}
	if err := puller.Pull(t.Context()); err != nil {
		t.Fatal(err)
	}
	old, err := mirror.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	next := *old
	next.Term.Epoch++
	next.Term.PrimaryID, next.Term.PromotionID = "new-primary", "new-promotion"
	if err := replication.Seal(&next); err != nil {
		t.Fatal(err)
	}
	calls := 0
	puller.Source = sourceFunc(func(context.Context, string) (Response, error) {
		calls++
		return Response{Term: next.Term, Image: &next, ETag: next.Generation().ETag()}, nil
	})
	contact := puller.Status().LastContact
	if err := puller.Pull(t.Context()); !errors.Is(err, replication.ErrCAS) {
		t.Fatal("original acquisition adopted newly observed authority", err)
	}
	if calls != 1 || !puller.Status().LastContact.Equal(contact) || mirror.Authority().Mode != replication.Fenced || !mirror.Authority().Term.Same(next.Term) {
		t.Fatal("higher source did not fence exactly once without contact credit")
	}
	got, err := mirror.ReadGeneration(t.Context())
	if err != nil || !got.Generation().Same(old.Generation()) {
		t.Fatal("refused original replaced last good image", err)
	}
	// A later independently captured fetch may use the already durable term.
	if err := puller.Pull(t.Context()); err != nil {
		t.Fatal("fresh operation could not install authenticated successor", err)
	}
	if calls != 2 || !mirror.Authority().Installed.Same(next.Generation()) {
		t.Fatal("fresh operation did not install exact successor")
	}
}

func TestPullOriginalLifetimeBoundsSourceAndReturnedImage(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded-default", true: "earlier-request"}[earlier], func(t *testing.T) {
			primary := open(t, replication.Primary)
			mirror := open(t, replication.Follower)
			if err := mirror.Enroll(t.Context(), primary.Authority().Term); err != nil {
				t.Fatal(err)
			}
			duration := time.Hour
			if earlier {
				duration = 5 * time.Second
			}
			original, cancel := context.WithTimeout(t.Context(), duration)
			defer cancel()
			deadline, _ := original.Deadline()
			calls := 0
			puller := &Puller{Mirror: mirror, Timeout: time.Hour, Source: sourceFunc(func(ctx context.Context, _ string) (Response, error) {
				calls++
				bound, ok := ctx.Deadline()
				if !ok || bound.After(time.Now().Add(30*time.Second)) || earlier && !bound.Equal(deadline) {
					t.Fatal("source received renewed or unbounded lifetime")
				}
				response, err := (directSource{primary}).Fetch(ctx, "")
				cancel() // Original expires after acquisition, before acceptance.
				return response, err
			})}
			if err := puller.Pull(original); !errors.Is(err, context.Canceled) {
				t.Fatal("returned image replaced original canceled lifetime", err)
			}
			if calls != 1 || !puller.Status().LastContact.IsZero() || mirror.Authority().Installed != (replication.Generation{}) {
				t.Fatal("canceled acquisition installed, retried, or acknowledged")
			}
		})
	}
}
