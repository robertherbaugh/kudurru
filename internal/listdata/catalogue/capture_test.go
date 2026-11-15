package catalogue

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
)

func captureDocument(t *testing.T, version, origin string) *Catalogue {
	t.Helper()
	raw := fmt.Appendf(nil, `{"schemaVersion":2,"version":%q,"license":"Apache-2.0",
"categories":[{"id":"video","name":"Video","order":1}],
"services":[
{"id":"alpha","name":"Alpha","category":"video","rules":["||alpha.example^"],"source":"test","requires":["beta"],"related":["beta"]},
{"id":"beta","name":"Beta","category":"video","rules":["||beta.example^"],"source":"test"}],
"safeSearch":[
{"id":"names","name":"Names","target":"safe.example","moderateTarget":"moderate.example","domains":["search.example"],"source":"test"},
{"id":"addresses","name":"Addresses","addresses":["192.0.2.1"],"domains":["addresses.example"],"source":"test"}]}`, version)
	c, err := Parse(raw, origin)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func captureImport(t *testing.T, name, origin string) *Import {
	t.Helper()
	raw := fmt.Appendf(nil, `{"blocked_services":[
{"id":"alpha","name":"Shadow","group":"streaming","rules":["||shadow.example^"]},
{"id":"gamma","name":%q,"group":"streaming","rules":["||gamma.example^"]}]}`, name)
	imp, err := ParseAdGuard(raw, origin)
	if err != nil {
		t.Fatal(err)
	}
	return imp
}

func expectOpen(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected installation wake")
	default:
	}
}

func expectClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatal("replacement did not broadcast")
	}
}

func TestHolderCaptureExactSourcesDigestNoopAndBroadcast(t *testing.T) {
	h := NewHolder(nil)
	c := h.Capture()
	legacy := h.Subscribe()
	a, b := h.Changes(), h.Changes()
	if !c.Current() || c.Identity() == "" || len(c.Identity()) > 128 || c.Catalogue() == Embedded() || c.Sources() != (Sources{BaseOrigin: OriginEmbedded, BaseDigest: Embedded().Digest()}) {
		t.Fatal("explicit embedded installation missing/did not detach")
	}
	urlCopy, err := Parse(Embedded().Raw(), "https://custom.example/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if h.Set(urlCopy) || h.Set(nil) || h.SetImport(nil) || !c.Current() || h.Capture().Identity() != c.Identity() || h.Capture().Sources().BaseOrigin != OriginEmbedded {
		t.Fatal("digest no-op relabeled or replaced embedded source")
	}
	expectOpen(t, a)
	expectOpen(t, legacy)
	base := captureDocument(t, "A", "https://source.example/A")
	if !h.Set(base) || c.Current() || h.Current() != base || h.Base() != base {
		t.Fatal("base replacement lost legacy pointers or exact invalidation")
	}
	expectClosed(t, a)
	expectClosed(t, b)
	expectClosed(t, legacy)
	c = h.Capture()
	same, err := Parse(base.Raw(), "https://different.example/A")
	if err != nil {
		t.Fatal(err)
	}
	a = h.Changes()
	if h.Set(same) || !c.Current() || c.Sources().BaseOrigin != base.Origin() {
		t.Fatal("same digest adopted a new source")
	}
	expectOpen(t, a)
	imp := captureImport(t, "Gamma", "https://imports.example/A")
	if !h.SetImport(imp) || c.Current() || h.Import() != imp {
		t.Fatal("import did not replace installation")
	}
	expectClosed(t, a)
	c = h.Capture()
	sources := Sources{BaseOrigin: base.Origin(), BaseDigest: base.Digest(), ImportOrigin: imp.Origin(), ImportDigest: imp.Digest(), HasImport: true}
	if c.Sources() != sources {
		t.Fatal("mixed provenance", c.Sources())
	}
	sameImp, err := ParseAdGuard(imp.Raw(), "https://different.example/import")
	if err != nil {
		t.Fatal(err)
	}
	a = h.Changes()
	if h.SetImport(sameImp) || !c.Current() || h.Capture().Sources() != sources {
		t.Fatal("import digest no-op relabeled source")
	}
	for range 100 {
		_ = h.Capture()
		_ = h.Changes()
	}
	if len(h.subs) != 1 {
		t.Fatal("capture/broadcast leaked legacy Subscribe registrations")
	}
	expectOpen(t, a)
	if !h.SetImport(nil) || c.Current() || h.Current() != base || h.Capture().Sources().HasImport || h.Capture().Catalogue().HasService("gamma") {
		t.Fatal("import removal failed exact replacement/base restoration")
	}
	expectClosed(t, a)
}

func TestHolderCaptureDetachedInputsLegacyViewsAndCompileClosures(t *testing.T) {
	base := captureDocument(t, "A", "https://source.example/A")
	imp := captureImport(t, "Gamma", "https://imports.example/A")
	h := NewHolder(base)
	if !h.SetImport(imp) {
		t.Fatal("import")
	}
	c := h.Capture()
	before, sources := c.Catalogue(), c.Sources()
	// Mutate all legacy-exposed nested slices after copying has completed.
	base.Services[0].Rules[0] = "||changed-base.example^"
	base.Services[0].Requires[0] = "changed"
	base.Services[0].Related[0] = "changed"
	base.Categories[0].Name = "changed"
	base.SafeSearch[0].Addresses[0] = "192.0.2.99"
	base.SafeSearch[0].addrs[0] = netip.MustParseAddr("192.0.2.99")
	base.SafeSearch[1].Domains[0] = "changed.example"
	imp.Services[1].Rules[0] = "||changed-import.example^"
	h.Current().Services[0].Name = "changed legacy merge"
	legacyMembers, _ := h.Current().CategoryServices("video")
	legacyMembers[0] = "changed"
	assertCapture := func(view *Catalogue) {
		t.Helper()
		alpha, ok := view.Service("alpha")
		gamma, gok := view.Service("gamma")
		if !ok || !gok || alpha.Name != "Alpha" || alpha.Rules[0] != "||alpha.example^" || alpha.Requires[0] != "beta" || alpha.Related[0] != "beta" || gamma.Rules[0] != "||gamma.example^" || view.Categories[0].Name != "Video" {
			t.Fatal("legacy/input mutation reached private captured data")
		}
		members, _ := view.CategoryServices("video")
		if !slices.Equal(members, []string{"alpha", "beta", "gamma"}) {
			t.Fatal("captured index borrowed mutable legacy membership", members)
		}
		r, ok := view.RewriteFor("search.example", ModeModerate)
		if !ok || r.Target != "moderate.example" {
			t.Fatal("captured safe-search domains changed", r)
		}
		r, ok = view.RewriteFor("addresses.example", ModeStrict)
		if !ok || !slices.Equal(r.Addrs, []netip.Addr{netip.MustParseAddr("192.0.2.1")}) {
			t.Fatal("captured address cache borrowed input", r)
		}
		snap, reports := blocking.Rebuild(nil, view.Specs(), blocking.CompileOptions{})
		for _, report := range reports {
			if report.Err != nil {
				t.Fatal(report.Err)
			}
		}
		if snap.Contains("alpha.example")[ListName("alpha")] != "||alpha.example^" || snap.Contains("gamma.example")[ListName("gamma")] != "||gamma.example^" || len(snap.Contains("changed-base.example")) != 0 {
			t.Fatal("compile closures borrowed mutable legacy rules")
		}
	}
	assertCapture(before)
	assertCapture(c.Catalogue())
	// Returned copies are independent of both the frozen data and each other.
	before.Services[0].Rules[0] = "||caller-copy.example^"
	before.imported.Services[0].Rules[0] = "||caller-copy.example^"
	before.SafeSearch[0].addrs[0] = netip.MustParseAddr("192.0.2.100")
	before.members["video"][0] = "caller"
	assertCapture(c.Catalogue())
	if !c.Current() || c.Sources() != sources {
		t.Fatal("legacy mutation replaced or relabeled installation")
	}
	// A later import must merge against private frozen BASE, not changed legacy
	// slices; a later base must retain the original frozen import.
	if !h.SetImport(captureImport(t, "Gamma B", "https://imports.example/B")) {
		t.Fatal("new import")
	}
	if alpha, _ := h.Capture().Catalogue().Service("alpha"); alpha.Rules[0] != "||alpha.example^" {
		t.Fatal("later import used mutated legacy base")
	}
	if !h.Set(captureDocument(t, "B", "https://source.example/B")) {
		t.Fatal("new base")
	}
	assertCapture(c.Catalogue()) // held installation survives both replacements
}

func TestHolderCapturePinAdmissionCancellationFailureAndABA(t *testing.T) {
	h := NewHolder(captureDocument(t, "A", "A"))
	c := h.Capture()
	other := NewHolder(c.Catalogue())
	var nilHolder *Holder
	if nilHolder.Capture().Current() || nilHolder.Capture().Catalogue() != nil || nilHolder.Changes() != nil || nilHolder.Capture().Sources() != (Sources{}) {
		t.Fatal("nil holder fabricated capability")
	}
	called := false
	fn := func() error { called = true; return nil }
	for _, tc := range []struct {
		h *Holder
		c Cut
	}{{nilHolder, c}, {h, Cut{}}, {h, other.Capture()}, {&Holder{}, c}} {
		if err := tc.h.WithCurrent(t.Context(), tc.c, fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
			t.Fatal("invalid/wrong-holder capability entered", err)
		}
	}
	if err := h.WithCurrent(t.Context(), c, nil); !errors.Is(err, snapshotowner.ErrInvalid) {
		t.Fatal("nil callback", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.WithCurrent(canceled, c, fn); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled callback entered", err)
	}
	failure := errors.New("callback failed")
	if err := h.WithCurrent(t.Context(), c, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal("callback error lost", err)
	}
	func() {
		defer func() {
			if recover() != "test panic" {
				t.Fatal("callback panic not propagated")
			}
		}()
		_ = h.WithCurrent(t.Context(), c, func() error { panic("test panic") })
	}()
	if err := h.WithCurrent(t.Context(), c, func() error { return nil }); err != nil {
		t.Fatal("failure/panic leaked gate", err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- h.WithCurrent(t.Context(), c, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	waiting, cancelWaiting := context.WithCancel(t.Context())
	waiter := make(chan error, 1)
	go func() { waiter <- h.WithCurrent(waiting, c, fn) }()
	cancelWaiting()
	if err := <-waiter; !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled waiter entered", err)
	}
	installed := make(chan bool, 1)
	go func() { installed <- h.Set(captureDocument(t, "B", "B")) }()
	select {
	case <-installed:
		t.Fatal("installation crossed pin")
	case <-time.After(20 * time.Millisecond):
	}
	// Live legacy reads/capture do not wait for a preparer blocked on the pin.
	reads := make(chan bool, 1)
	go func() {
		reads <- h.Current() != nil && h.Base() != nil && h.Import() == nil && h.Capture().Current() && c.Catalogue().Version == "A"
	}()
	select {
	case ok := <-reads:
		if !ok {
			t.Fatal("pin changed capture/read results")
		}
	case <-time.After(time.Second):
		t.Fatal("legacy read took writer preparation mutex")
	}
	if h.Set(c.Catalogue()) {
		t.Fatal("digest no-op while pinned replaced installation")
	}
	unblock()
	if err := <-done; err != nil || !<-installed {
		t.Fatal("pin/install completion", err)
	}
	if c.Current() || !h.Set(c.Catalogue()) || c.Current() || h.Capture().Identity() == c.Identity() {
		t.Fatal("A-B-A revived old installation")
	}
	if err := h.WithCurrent(t.Context(), c, fn); !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("stale A admitted after ABA", err)
	}
}

func TestHolderCaptureConcurrentBaseImportNoLostMerge(t *testing.T) {
	for range 20 {
		h := NewHolder(captureDocument(t, "A", "A"))
		base, imp := captureDocument(t, "B", "B"), captureImport(t, "Gamma", "import")
		start, done := make(chan struct{}), make(chan bool, 2)
		go func() { <-start; done <- h.Set(base) }()
		go func() { <-start; done <- h.SetImport(imp) }()
		close(start)
		baseChanged, importChanged := <-done, <-done
		if !baseChanged || !importChanged {
			t.Fatal("concurrent update lost")
		}
		c := h.Capture()
		if !c.Current() || c.Sources() != (Sources{BaseOrigin: "B", BaseDigest: base.Digest(), ImportOrigin: "import", ImportDigest: imp.Digest(), HasImport: true}) || c.Catalogue().Version != "B" || !c.Catalogue().HasService("gamma") {
			t.Fatal("concurrent base/import produced mixed cut")
		}
		if alpha, _ := c.Catalogue().Service("alpha"); alpha.Name != "Alpha" {
			t.Fatal("import overrode base precedence")
		}
	}
}

func TestHolderCaptureRejectsUnboundedMutatedInputs(t *testing.T) {
	h := NewHolder(nil)
	c, changed := h.Capture(), h.Changes()
	for _, edit := range []func(*Catalogue){
		func(c *Catalogue) { c.Services = make([]Service, 2*maxServices+1) },
		func(c *Catalogue) { c.Services[0].Rules = make([]string, maxRules+1) },
		func(c *Catalogue) { c.Services[0].Requires = make([]string, maxHints+1) },
		func(c *Catalogue) { c.SafeSearch[0].Domains = make([]string, maxDomains+1) },
		func(c *Catalogue) { c.SafeSearch[0].Addresses = make([]string, maxCaptureAddresses+1) },
		func(c *Catalogue) { c.Homepage = strings.Repeat("x", MaxBytes+MaxAdGuardBytes+1) },
		func(c *Catalogue) { c.origin = strings.Repeat("x", maxCaptureOriginBytes+1) },
	} {
		bad := captureDocument(t, "B", "B")
		edit(bad)
		if h.Set(bad) || !c.Current() {
			t.Fatal("oversized public mutation admitted or invalidated last good")
		}
		if cut := NewHolder(bad).Capture(); cut.Current() || cut.Catalogue() != nil {
			t.Fatal("oversized constructor granted captured capability")
		}
	}
	badImport := captureImport(t, "B", "B")
	badImport.Services = make([]Service, maxServices+1)
	if h.SetImport(badImport) || !c.Current() {
		t.Fatal("oversized import admitted")
	}
	expectOpen(t, changed)
}

func TestHolderCaptureShadowedImportReplacementStillInvalidates(t *testing.T) {
	h := NewHolder(captureDocument(t, "A", "A"))
	parse := func(name string) *Import {
		t.Helper()
		imp, err := ParseAdGuard(fmt.Appendf(nil, `[{"id":"alpha","name":%q,"group":"streaming","rules":["||shadow.example^"]}]`, name), "import")
		if err != nil {
			t.Fatal(err)
		}
		return imp
	}
	if !h.SetImport(parse("Shadow A")) {
		t.Fatal("initial shadowed import")
	}
	c, changed := h.Capture(), h.Changes()
	fp := c.Catalogue().Fingerprint()
	if !h.SetImport(parse("Shadow B")) || c.Current() || h.Capture().Catalogue().Fingerprint() == fp {
		t.Fatal("shadowed import content failed to invalidate dependency")
	}
	expectClosed(t, changed)
	alpha, _ := h.Capture().Catalogue().Service("alpha")
	if alpha.Name != "Alpha" {
		t.Fatal("shadowed import overrode base")
	}
	if _, count := h.Capture().Catalogue().Imported(); count != 1 {
		t.Fatal("shadowed count lost", count)
	}
}

func TestHolderCaptureConcurrentReadersBindOneInstallation(t *testing.T) {
	a, b := captureDocument(t, "A", "source-A"), captureDocument(t, "B", "source-B")
	imp := captureImport(t, "Gamma", "import")
	h := NewHolder(a)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			h.Set(b)
			h.SetImport(imp)
			h.Set(a)
			h.SetImport(nil)
		}
	}()
	for range 400 {
		c := h.Capture()
		source, model := c.Sources(), c.Catalogue()
		if c.Identity() == "" || model == nil || model.Digest() != source.BaseDigest || model.Origin() != source.BaseOrigin ||
			(model.Version == "A") != (source.BaseOrigin == "source-A") || model.HasService("gamma") != source.HasImport {
			t.Fatal("atomic cut mixed base/import/source installations")
		}
		got, _ := model.Imported()
		if source.HasImport && (got == nil || got.Digest() != source.ImportDigest || got.Origin() != source.ImportOrigin) || !source.HasImport && got != nil {
			t.Fatal("atomic cut mixed import provenance")
		}
	}
	<-done
}
