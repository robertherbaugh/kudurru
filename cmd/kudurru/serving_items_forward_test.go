package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A write that an enrolled follower forwards is a command of the primary's
// coordinator like any other. Between two items of a request that runs on the
// primary itself it is a foreign commit, also under the same credential.
func TestServingItemsForwardedFollowerWriteRefusesLaterItems(t *testing.T) {
	h := newRootMemberHarness(t)
	f := &itemsFixture{servingCommandFixture: h.f}
	f.mount(t, f.http)
	f.between = func(n int, _ context.Context) {
		if n != 1 {
			return
		}
		group := servingObjectGroup("from-follower")
		group.TypeMeta = metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "ClientGroup"}
		document, err := json.Marshal(group)
		if err != nil {
			t.Error(err)
			return
		}
		status, out := h.send(h.command("between-two-items", memberObjectWrite, api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "ClientGroup", Namespace: "default", Name: "from-follower", Document: document}))
		var result api.ForwardedObjectResult
		if status != http.StatusOK || out.Status != http.StatusOK || json.Unmarshal(out.Body, &result) != nil || result.Result != api.ResultCreated {
			t.Error("the forwarded write was not committed", status, out.Status, string(out.Body))
		}
	}
	results := f.apply(t, f.server, "local-a", "local-b", "local-c")
	if results[0].Result != api.ResultCreated {
		t.Fatalf("the item before the forwarded write was not applied: %s", itemsResults(results))
	}
	for _, result := range results[1:] {
		if result.Result != api.ResultError || result.ErrorKey != api.KeyUnavailable || result.Error != itemsForeign {
			t.Fatalf("an item after a forwarded write was not refused with the documented answer: %s", itemsResults(results))
		}
	}
	if !f.exists(t, "local-a") || !f.exists(t, "from-follower") || f.exists(t, "local-b") || f.exists(t, "local-c") || f.commands != 3 {
		t.Fatal("a refused item was applied or replayed, or a committed one was lost", f.commands)
	}
}

// Why repeating a cut multi-document apply through a follower does not finish
// it: the primary commits a deduplication record for every forwarded command,
// also for a document that is already applied and answers unchanged. The
// follower's next pull therefore installs a newer image during every repeat.
func TestRootMemberUnchangedForwardedWriteStillCommits(t *testing.T) {
	h := newRootMemberHarness(t)
	group := servingObjectGroup("repeated")
	group.TypeMeta = metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "ClientGroup"}
	document, err := json.Marshal(group)
	if err != nil {
		t.Fatal(err)
	}
	forward := func(id, want string) uint64 {
		t.Helper()
		before := h.f.s.opts.Store.Authority().Installed.Revision
		status, out := h.send(h.command(id, memberObjectWrite, api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "ClientGroup", Namespace: "default", Name: "repeated", Document: document}))
		var result api.ForwardedObjectResult
		if status != http.StatusOK || out.Status != http.StatusOK || json.Unmarshal(out.Body, &result) != nil || result.Result != want {
			t.Fatalf("forwarded write %s: status %d, outcome %d %s, want %s", id, status, out.Status, out.Body, want)
		}
		return h.f.s.opts.Store.Authority().Installed.Revision - before
	}
	if commits := forward("first-run", api.ResultCreated); commits != 1 {
		t.Fatal("the forwarded create did not commit once", commits)
	}
	if commits := forward("second-run", api.ResultUnchanged); commits != 1 {
		t.Fatal("a forwarded document that changes nothing did not commit its command record", commits)
	}
}
