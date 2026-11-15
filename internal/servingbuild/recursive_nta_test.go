package servingbuild

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestCompletePublicationCapturesRecursiveNTA(t *testing.T) {
	f := newFixture(t)
	defer f.stop()
	opts := f.builder.opts
	prepare := func(expiry time.Time) *semanticOwners {
		t.Helper()
		settings, err := json.Marshal(map[string]any{
			"groups":    map[string]any{"default": map[string]any{"mode": "recursive", "servers": []string{}}},
			"recursion": map[string]any{"ipv6": "off", "negativeTrustAnchors": []map[string]any{{"domain": "broken.example.", "reason": "incident", "expiresAt": expiry}}},
		})
		if err != nil {
			t.Fatal("fixture settings did not encode")
		}
		f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var obj public.Kudurru
			if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &obj); err != nil {
				return err
			}
			obj.Spec.Settings["upstreams"] = extensions.JSON{Raw: settings}
			_, err := tx.Apply(ctx, &obj, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		image, token, err := f.store.ReadServingCandidate(t.Context())
		if err != nil {
			t.Fatal("fixture candidate capture failed")
		}
		deps, err := opts.Capture(t.Context(), image)
		if err != nil {
			t.Fatal("fixture dependency capture failed")
		}
		defer deps.Release()
		owner, err := prepareSemantic(t.Context(), serving.Candidate{Image: image, Token: token}, opts, deps, f.now)
		if err != nil {
			t.Fatal("complete recursive NTA publication failed")
		}
		t.Cleanup(owner.release)
		return owner
	}
	expiry := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	a := prepare(expiry)
	b := prepare(expiry.Add(time.Hour))
	first, aUntil := a.upstreams.Recursor().TrustContext()
	second, bUntil := b.upstreams.Recursor().TrustContext()
	if first == second || a.upstreams.Identity() == b.upstreams.Identity() || a.upstreams.Recursor() == b.upstreams.Recursor() {
		t.Fatalf("complete recursive context: same_context=%t same_set=%t same_resolver=%t config_counts=%d/%d native_counts=%d/%d", first == second, a.upstreams.Identity() == b.upstreams.Identity(), a.upstreams.Recursor() == b.upstreams.Recursor(), len(a.config.Upstreams.Recursion.NegativeTrustAnchors), len(b.config.Upstreams.Recursion.NegativeTrustAnchors), len(a.upstreams.Recursor().Config().NegativeTrustAnchors), len(b.upstreams.Recursor().Config().NegativeTrustAnchors))
	}
	if !aUntil.Equal(expiry) || !bUntil.Equal(expiry.Add(time.Hour)) || !a.config.Upstreams.Recursion.NegativeTrustAnchors[0].ExpiresAt.Equal(expiry) {
		t.Fatal("new publication renewed a held recursive exception")
	}
}
