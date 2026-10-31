package servingbuild

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestRegexImpactActualSQLiteRetainsOriginalPublication(t *testing.T) {
	f := newFixture(t)
	allow := convenienceDomainSubject(false, "original")
	allow.Spec.Sources[0].Entries = []public.ListEntry{{Type: public.EntryRegex, Value: `^alpha\.example$`}}
	block := convenienceDomainSubject(false, "block")
	block.Name, block.Spec.Mode = "other-block", "block"
	for _, obj := range []store.Object{allow, block} {
		if _, err := f.store.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	f.waitCurrent(t)
	held := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 1), resume: make(chan struct{})}
	held.remaining.Store(1)
	server := convenienceReadHTTP(t, f, f.pub, held, nil)
	cleanupHeldObjectReads(t, held)
	path := "allowlist/" + domainlist.ID("default", public.EntryRegex, allow.Spec.Sources[0].Entries[0].Value) + "/impact?name=alpha.example"
	done := make(chan objectReadResponse, 1)
	go func() { done <- objectReadGET(t.Context(), server, f.token, path) }()
	objectReadWait(t, held)
	block.Spec.Sources[0].Entries = []public.ListEntry{{Type: public.EntryExact, Value: "beta.example"}}
	if _, err := f.store.Apply(t.Context(), block, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	close(held.resume)
	decode := func(response objectReadResponse) apihttp.DomainRegexImpact {
		t.Helper()
		var result apihttp.DomainRegexImpact
		if err := json.Unmarshal([]byte(diagnosticWant(t, response.diagnosticResponse, http.StatusOK)), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	original := decode(<-done)
	fresh := decode(objectReadGET(t.Context(), server, f.token, path))
	if !original.Matches || len(original.Overlaps) != 1 || original.Overlaps[0].List != "default/other-block" || !fresh.Matches || len(fresh.Overlaps) != 0 {
		t.Fatal("diagnostic mixed captured entry and successor lists", original, fresh)
	}
}
