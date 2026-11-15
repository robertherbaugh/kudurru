package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestServingHTTPCommandCommitPublishesItsRevisionOnce proves D407: a current
// command whose commit advanced the generation does not Wake the publisher on
// top of it. With that Wake, the publisher answered the commit's generation
// change with one publication and, whenever the Wake arrived after that
// preparation had read its candidate, published the same revision a second
// time (D405); the second publication refused, as superseded, every command
// captured on the first one (TestServingHTTPRoleBindingsReviewedCRUD answered
// such a command 503 instead of 409).
func TestServingHTTPCommandCommitPublishesItsRevisionOnce(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	localUsersFresh(t, f)
	var (
		mu sync.Mutex
		// seen maps a served revision and dependency identity to the
		// publication sequence numbers that served it. A dependency change
		// (keyring, catalog, transferred zone) may publish a revision again
		// with other dependencies; the same cut published twice is the fault.
		seen = map[string]map[uint64]bool{}
	)
	record := func() {
		served := f.s.publisher.Status().Served
		if served.Publication == 0 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		cut := fmt.Sprintf("revision %d dependencies %q", served.Base.Revision, served.Dependencies)
		if seen[cut] == nil {
			seen[cut] = map[uint64]bool{}
		}
		seen[cut][served.Publication] = true
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			changed := f.s.publisher.Subscribe()
			record()
			select {
			case <-done:
				return
			case <-changed:
			case <-time.After(time.Millisecond):
			}
		}
	}()
	defer func() { close(done); <-stopped }()
	// idle waits until the publisher has prepared nothing for a while, so
	// that a second publication of the same revision would have been seen.
	idle := func() {
		t.Helper()
		quiet := time.Now()
		for deadline := time.Now().Add(30 * time.Second); time.Since(quiet) < 750*time.Millisecond; {
			if time.Now().After(deadline) {
				t.Fatal("the publisher did not settle")
			}
			if f.s.publisher.Status().Preparing {
				quiet = time.Now()
			}
			time.Sleep(time.Millisecond)
		}
	}
	idle()
	for i := range 8 {
		in := roleBindingManifest(fmt.Sprintf("wake-%d", i), "viewer", "alice")
		roleBindingResponse(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, in), f.token, nil, ""), 201)
		localUsersFresh(t, f)
		idle()
	}
	mu.Lock()
	defer mu.Unlock()
	for cut, publications := range seen {
		if len(publications) > 1 {
			t.Errorf("%s was published %d times: %v", cut, len(publications), publications)
		}
	}
	if len(seen) < 8 {
		t.Fatalf("saw %d published revisions, want at least the 8 commits", len(seen))
	}
}
