package servinghttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// unrelatedCommit changes nothing the authenticated request depends on, like
// the zone lifecycle driver's status writes.
func unrelatedCommit(t *testing.T, f *fixture, name string) {
	t.Helper()
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &publicv1.BlockList{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: publicv1.BlockListSpec{Mode: "block", Sources: []publicv1.ListSource{{Inline: []string{name + ".example"}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
}

// recaptureIn runs one cookie-authenticated request through the middleware
// and calls inside with the request context. The request is never
// dispatched: inside decides what supersedes its publication and calls
// Recapture itself.
func recaptureIn(t *testing.T, f *fixture, inside func(ctx context.Context)) {
	t.Helper()
	called := false
	r := f.request(http.MethodPost)
	r.Header.Set(auth.CSRFHeader, f.csrf)
	recorder := httptest.NewRecorder()
	f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		inside(r.Context())
	})).ServeHTTP(recorder, r)
	if !called {
		t.Fatalf("request did not reach its handler: %d %s", recorder.Code, recorder.Body)
	}
}

func identityOf(ctx context.Context, t *testing.T) serving.Identity {
	t.Helper()
	identity, err := Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func recaptureFor(ctx context.Context, f *fixture, bound time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return Recapture(ctx, wait, f.publisher, func() time.Time { return f.now })
}

// Each case below supersedes the request's publication before any command
// of it dispatched. Recapture admits the new publication only when nothing
// the request depends on changed (D399); every refusal leaves the request on
// its original lease, so the caller answers its original effect-free 503.
func TestRecaptureAdmitsOnlyAnUnchangedRequest(t *testing.T) {
	t.Run("unrelated commit is admitted with the same credential", func(t *testing.T) {
		f := newFixture(t)
		recaptureIn(t, f, func(ctx context.Context) {
			before := identityOf(ctx, t)
			unrelatedCommit(t, f, "admitted")
			if err := recaptureFor(ctx, f, 5*time.Second); err != nil {
				t.Fatal("unchanged request not recaptured", err)
			}
			after := identityOf(ctx, t)
			if after.Base.Revision <= before.Base.Revision || after.Publication <= before.Publication {
				t.Fatalf("request still on its superseded publication: %+v -> %+v", before, after)
			}
			if _, err := Credential(ctx); err != nil {
				t.Fatal("proof lost across recapture", err)
			}
		})
	})
	t.Run("cookie session revoked by the superseding commit", func(t *testing.T) {
		f := newFixture(t)
		recaptureIn(t, f, func(ctx context.Context) {
			before := identityOf(ctx, t)
			f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
				s, err := security.Load(ctx, tx)
				if err != nil {
					return err
				}
				if err := s.Revoke(f.row.ID); err != nil {
					return err
				}
				return s.Commit(ctx)
			})
			if err := recaptureFor(ctx, f, 5*time.Second); !errors.Is(err, ErrNotRecaptured) {
				t.Fatal("a revoked session was recaptured", err)
			}
			if identityOf(ctx, t) != before {
				t.Fatal("a refused recapture replaced the request's lease")
			}
		})
	})
	t.Run("authority fenced while the request waits", func(t *testing.T) {
		f := newFixture(t)
		recaptureIn(t, f, func(ctx context.Context) {
			before := identityOf(ctx, t)
			term := f.store.Authority().Term
			term.Epoch++
			term.PrimaryID, term.PromotionID, term.PrimaryName = "other-member", "other-promotion", "node-b"
			if _, err := f.store.ObserveAuthority(t.Context(), term); err != nil {
				t.Fatal(err)
			}
			if f.store.Authority().Mode != replication.Fenced {
				t.Fatal("fixture is not fenced")
			}
			if err := recaptureFor(ctx, f, 500*time.Millisecond); err == nil {
				t.Fatal("a fenced request was recaptured")
			}
			if identityOf(ctx, t) != before {
				t.Fatal("a refused recapture replaced the request's lease")
			}
		})
	})
	t.Run("exact source refuses a commit that changed the source", func(t *testing.T) {
		f := newFixture(t)
		recaptureIn(t, f, func(ctx context.Context) {
			ctx = WithExactSource(ctx)
			before := identityOf(ctx, t)
			unrelatedCommit(t, f, "exact-source")
			if err := recaptureFor(ctx, f, 5*time.Second); !errors.Is(err, ErrNotRecaptured) {
				t.Fatal("an exact-source request was recaptured on another source generation", err)
			}
			if identityOf(ctx, t) != before {
				t.Fatal("a refused recapture replaced the request's lease")
			}
		})
	})
	t.Run("exact source admits a republication of the same source", func(t *testing.T) {
		f := newFixture(t)
		recaptureIn(t, f, func(ctx context.Context) {
			ctx = WithExactSource(ctx)
			before := identityOf(ctx, t)
			f.publisher.Wake()
			if err := recaptureFor(ctx, f, 5*time.Second); err != nil {
				t.Fatal("a republication of the same source was refused", err)
			}
			after := identityOf(ctx, t)
			if after.Base != before.Base || after.Publication <= before.Publication {
				t.Fatalf("want a new publication of the same source: %+v -> %+v", before, after)
			}
		})
	})
	t.Run("after the request dispatched a command", func(t *testing.T) {
		f := newFixture(t)
		recaptureIn(t, f, func(ctx context.Context) {
			before := identityOf(ctx, t)
			MarkDispatched(ctx)
			unrelatedCommit(t, f, "dispatched")
			if err := recaptureFor(ctx, f, 5*time.Second); !errors.Is(err, ErrNotRecaptured) {
				t.Fatal("a request captured again after its own dispatch", err)
			}
			if identityOf(ctx, t) != before {
				t.Fatal("a refused recapture replaced the request's lease")
			}
		})
	})
	t.Run("proof held by another request", func(t *testing.T) {
		f := newFixture(t)
		recaptureIn(t, f, func(ctx context.Context) {
			credential, err := Credential(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ctx = context.WithValue(ctx, proofKey{}, proof{holder: newRequestCell(nil), credential: credential})
			unrelatedCommit(t, f, "foreign-proof")
			if err := recaptureFor(ctx, f, 5*time.Second); !errors.Is(err, ErrNotRecaptured) {
				t.Fatal("a proof of another request was recaptured", err)
			}
		})
	})
	t.Run("caller marked without recapture", func(t *testing.T) {
		f := newFixture(t)
		recaptureIn(t, f, func(ctx context.Context) {
			before := identityOf(ctx, t)
			unrelatedCommit(t, f, "without")
			began := time.Now()
			if err := recaptureFor(WithoutRecapture(ctx), f, 5*time.Second); !errors.Is(err, ErrNotRecaptured) {
				t.Fatal("a caller marked without recapture was recaptured", err)
			}
			if took := time.Since(began); took > time.Second {
				t.Fatal("a caller marked without recapture waited", took)
			}
			if identityOf(ctx, t) != before {
				t.Fatal("a refused recapture replaced the request's lease")
			}
		})
	})
}
