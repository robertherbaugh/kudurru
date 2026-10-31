package servingbuild

import (
	"context"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestCompletePublicationOwnsRecursiveResolver(t *testing.T) {
	f := newFixture(t)
	defer f.stop()
	startup, err := recursor.New(recursor.DefaultConfig(), recursor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	opts := f.builder.opts
	opts.Upstreams.Recursor = startup
	prepare := func(hint string) *semanticOwners {
		t.Helper()
		f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var object public.Kudurru
			if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &object); err != nil {
				return err
			}
			object.Spec.Settings["upstreams"] = extensions.JSON{Raw: []byte(`{"groups":{"default":{"mode":"recursive","servers":[]}},"recursion":{"dnssec":false,"ipv6":"off","rootHints":["` + hint + `"]}}`)}
			_, err := tx.Apply(ctx, &object, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		image, token, err := f.store.ReadServingCandidate(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		deps, err := opts.Capture(t.Context(), image)
		if err != nil {
			t.Fatal(err)
		}
		defer deps.Release()
		owner, err := prepareSemantic(t.Context(), serving.Candidate{Image: image, Token: token}, opts, deps, f.now)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(owner.release)
		return owner
	}
	a := prepare("192.0.2.53")
	b := prepare("192.0.2.54")
	if a.upstreams.Recursor() == nil || b.upstreams.Recursor() == nil || a.upstreams.Recursor() == startup || b.upstreams.Recursor() == startup || a.upstreams.Recursor() == b.upstreams.Recursor() {
		t.Fatal("complete publications share mutable recursive settings")
	}
	if a.upstreams.Identity() == b.upstreams.Identity() {
		t.Fatal("root-hint replacement reused the cache scope")
	}
	if a.config.Upstreams.Recursion.RootHints[0] != "192.0.2.53" || b.config.Upstreams.Recursion.RootHints[0] != "192.0.2.54" {
		t.Fatal("held recursive settings changed")
	}
}
