package servinghttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type refusal struct {
	Error struct {
		Key     string `json:"key"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	} `json:"error"`
}

func refuse(t *testing.T, f *fixture) (*httptest.ResponseRecorder, refusal) {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.adapter.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a request without a complete lease reached its handler")
	})).ServeHTTP(recorder, f.request(http.MethodGet))
	var body refusal
	if recorder.Code != http.StatusServiceUnavailable || json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		t.Fatalf("capture refusal: %d %s", recorder.Code, recorder.Body)
	}
	return recorder, body
}

// A fenced or conflicted member closes its API for as long as its durable
// authority says so. The refusal names that state and what ends it and
// carries no Retry-After; a publication that is only being prepared keeps
// generation_pending with Retry-After.
func TestCaptureRefusalNamesFencedAndConflictedAuthority(t *testing.T) {
	observe := func(t *testing.T, f *fixture, epochs uint64, want error) {
		t.Helper()
		term := f.store.Authority().Term
		term.Epoch += epochs
		term.PrimaryID, term.PromotionID, term.PrimaryName = "other-member", "other-promotion", "node-b"
		if _, err := f.store.ObserveAuthority(t.Context(), term); !errors.Is(err, want) {
			t.Fatal("authority observation", err)
		}
	}
	t.Run("fenced", func(t *testing.T) {
		f := newFixture(t)
		observe(t, f, 1, nil)
		if f.store.Authority().Mode != replication.Fenced {
			t.Fatal("fixture is not fenced")
		}
		recorder, body := refuse(t, f)
		if body.Error.Key != KeyClusterFenced || !strings.Contains(body.Error.Message, "the newer primary node-b") ||
			!strings.Contains(body.Error.Hint, "current primary") || recorder.Header().Get("Retry-After") != "" {
			t.Fatalf("fenced refusal: retry-after=%q %s", recorder.Header().Get("Retry-After"), recorder.Body)
		}
	})
	t.Run("conflict", func(t *testing.T) {
		f := newFixture(t)
		observe(t, f, 0, replication.ErrPromotionConflict)
		if a := f.store.Authority(); a.Mode != replication.Conflict || a.Conflict == nil {
			t.Fatal("fixture is not in conflict")
		}
		recorder, body := refuse(t, f)
		if body.Error.Key != KeyClusterConflict || !strings.Contains(body.Error.Message, "conflicting promotions") ||
			!strings.Contains(body.Error.Hint, "kudurru cluster resolution --offline") || !strings.Contains(body.Error.Hint, "kudurru cluster resolve --offline") ||
			recorder.Header().Get("Retry-After") != "" {
			t.Fatalf("conflict refusal: retry-after=%q %s", recorder.Header().Get("Retry-After"), recorder.Body)
		}
	})
	t.Run("pending publication", func(t *testing.T) {
		f := newFixture(t)
		f.pause.Store(true)
		f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "pending"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "pending.example."}}, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		recorder, body := refuse(t, f)
		if body.Error.Key != "generation_pending" || body.Error.Hint != "" || recorder.Header().Get("Retry-After") != "1" {
			t.Fatalf("pending refusal: retry-after=%q %s", recorder.Header().Get("Retry-After"), recorder.Body)
		}
	})
}

// SourceRole names the primary of the source the request began on.
func TestSourceRoleNamesThePrimaryOfTheOriginalSource(t *testing.T) {
	f := newFixture(t)
	lease, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), leaseKey{}, lease))
	want := f.store.Authority()
	cancel()
	mode, primary, err := SourceRole(ctx)
	if err != nil || mode != want.Mode || primary != want.Term.PrimaryName {
		t.Fatal("ended request lost its source role", mode, primary, err)
	}
	if _, _, err := SourceRole(t.Context()); err == nil {
		t.Fatal("a request without a lease reported a role")
	}
}
