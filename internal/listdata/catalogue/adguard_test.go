package catalogue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
)

// adguardFixture is a hand-written document in the AdGuard
// HostlistsRegistry services format. It contains no AdGuard data: the
// services and domains are invented (example.com, example.net and the
// reserved .example TLD); youtube only collides with a kudurru ID on
// purpose.
const adguardFixture = `{
  "groups": [{"id": "streaming"}, {"id": "software"}],
  "blocked_services": [
    {"id": "example_stream", "name": "Example Stream", "group": "streaming", "icon_svg": "<svg/>",
     "rules": ["||stream.example^", "||cdn.stream.example^", "||bad rule^", "@@||allowed.example^", "||mod.example^$important", "example.com##.ad"]},
    {"id": "example_tool", "name": "Example Tool", "group": "software", "rules": ["||tool.example^"]},
    {"id": "example_chat", "name": "Example Chat", "group": "messenger", "rules": ["||chat.example.net^"]},
    {"id": "youtube", "name": "Not YouTube", "group": "streaming", "rules": ["||imported-youtube.example^"]},
    {"id": "Bad ID", "name": "Bad", "group": "ai", "rules": ["||bad.example^"]},
    {"id": "no_rules", "name": "No rules", "group": "ai", "rules": ["@@||x.example^"]},
    {"id": "example_tool", "name": "Duplicate", "group": "ai", "rules": ["||dup.example^"]},
    {"id": "nameless", "name": " ", "group": "ai", "rules": ["||nameless.example^"]}
  ]
}`

func TestParseAdGuard(t *testing.T) {
	imp, err := ParseAdGuard([]byte(adguardFixture), "https://mirror.example/services.json")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range imp.Services {
		ids = append(ids, s.ID)
		if s.Source != AdGuardSource || s.Origin() != OriginAdGuard {
			t.Errorf("%s: source %q origin %q", s.ID, s.Source, s.Origin())
		}
	}
	if want := []string{"example-chat", "example-stream", "example-tool", "youtube"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	byID := map[string]Service{}
	for _, s := range imp.Services {
		byID[s.ID] = s
	}
	if s := byID["example-stream"]; s.Category != "video" || !slices.Equal(s.Rules, []string{"||stream.example^", "||cdn.stream.example^"}) || s.Name != "Example Stream" {
		t.Errorf("example-stream = %+v", s)
	}
	if byID["example-chat"].Category != "messaging" || byID["example-tool"].Category != CategoryOther {
		t.Errorf("group mapping: chat %q, tool %q", byID["example-chat"].Category, byID["example-tool"].Category)
	}
	if imp.Skipped != 4 || imp.DroppedRules != 5 {
		t.Errorf("skipped %d, dropped rules %d", imp.Skipped, imp.DroppedRules)
	}
	if imp.Origin() != "https://mirror.example/services.json" || imp.Digest() != digestOf([]byte(adguardFixture)) {
		t.Errorf("origin %q digest %q", imp.Origin(), imp.Digest())
	}

	// A bare array is read too.
	if imp, err := ParseAdGuard([]byte(`[{"id":"a_b","name":"A","group":"ai","rules":["||a.example^"]}]`), "x"); err != nil || imp.Services[0].ID != "a-b" || imp.Services[0].Category != "ai" {
		t.Errorf("array: %+v, %v", imp, err)
	}
	for name, doc := range map[string]string{
		"not json":   `{`,
		"no key":     `{"services": []}`,
		"no service": `{"blocked_services": [{"id": "x", "name": "X", "rules": ["@@||x.example^"]}]}`,
		"empty":      `{"blocked_services": []}`,
	} {
		if _, err := ParseAdGuard([]byte(doc), "x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseAdGuard(make([]byte, MaxAdGuardBytes+1), "x"); err == nil {
		t.Error("an oversized document was accepted")
	}
}

// TestAdGuardRulesBlockOneSite: an imported service only keeps ||name^
// rules of one site, so a hostile or broken document cannot block every
// name for the Policies that block its category.
func TestAdGuardRulesBlockOneSite(t *testing.T) {
	doc := `{"blocked_services":[{"id":"evil_ai","name":"Evil","group":"ai","rules":[` +
		`"/.*/","||com^","||co.uk^","||github.io^","||arpa^","||in-addr.arpa^","*.com","||1.2.3.4^","127.0.0.1 a.com",` +
		`"||*.example.org^","example.org","/(a+)+$/","||evil.example^","||www.evil.co.uk^","||user.github.io^"]}]}`
	imp, err := ParseAdGuard([]byte(doc), "x")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := imp.Services[0].Rules, []string{"||evil.example^", "||www.evil.co.uk^", "||user.github.io^"}; !slices.Equal(got, want) {
		t.Errorf("rules = %q, want %q", got, want)
	}
	if imp.DroppedRules != 12 {
		t.Errorf("dropped %d rules, want 12", imp.DroppedRules)
	}
}

// TestMergeAdGuard: imported services join the catalogue, kudurru's entry
// wins on an ID conflict, unknown categories become "other", and the
// fingerprint changes with the import.
func TestMergeAdGuard(t *testing.T) {
	imp, err := ParseAdGuard([]byte(adguardFixture), "https://mirror.example/services.json")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHolder(nil)
	ch := h.Subscribe()
	base := h.Current()
	if !h.SetImport(imp) || h.SetImport(imp) {
		t.Fatal("SetImport reported the wrong change")
	}
	select {
	case <-ch:
	default:
		t.Error("subscriber not notified")
	}
	c := h.Current()
	if c.Fingerprint() == base.Fingerprint() || c.Digest() != base.Digest() || h.Base() != base {
		t.Errorf("fingerprint %q digest %q", c.Fingerprint(), c.Digest())
	}
	if got, shadowed := c.Imported(); got != imp || shadowed != 1 {
		t.Errorf("Imported = %v, %d", got, shadowed)
	}
	if len(c.Services) != len(base.Services)+3 {
		t.Errorf("%d services, want %d", len(c.Services), len(base.Services)+3)
	}
	yt, _ := c.Service("youtube")
	if yt.Name != "YouTube" || yt.Origin() != OriginKudurru {
		t.Errorf("the kudurru youtube must win: %+v", yt)
	}
	s, ok := c.Service("example-stream")
	if !ok || s.Origin() != OriginAdGuard || s.Category != "video" {
		t.Errorf("example-stream = %+v", s)
	}
	if ids, _ := c.CategoryServices("video"); !slices.Contains(ids, "example-stream") {
		t.Errorf("video = %v", ids)
	}
	if ids, ok := c.CategoryServices(CategoryOther); !ok || !slices.Equal(ids, []string{"example-tool"}) {
		t.Errorf("other = %v, %v", ids, ok)
	}
	specs, fp := h.Lists()
	if fp != "catalogue:"+c.Fingerprint() || len(specs) != len(c.Services) {
		t.Errorf("Lists = %d specs, %q", len(specs), fp)
	}
	// A base without "other" gets one for the import.
	small, err := Parse(mutate(t, func(d map[string]any) {
		d["categories"] = []any{map[string]any{"id": "video", "name": "Video", "order": 1}}
		d["services"] = []any{map[string]any{"id": "youtube", "name": "YouTube", "category": "video", "rules": []any{"||youtube.com^"}, "source": "test"}}
	}), "test")
	if err != nil {
		t.Fatal(err)
	}
	m := merge(small, imp)
	if !m.HasCategory(CategoryOther) || !m.HasService("example-chat") {
		t.Errorf("merged categories = %v", m.Categories)
	}
	if svc, _ := m.Service("example-chat"); svc.Category != CategoryOther {
		t.Errorf("example-chat without a messaging category = %q", svc.Category)
	}
	// Removing the import restores the base.
	if !h.SetImport(nil) || h.Current() != base {
		t.Error("SetImport(nil) did not restore the catalogue document")
	}
	// A new base document keeps the import merged.
	h.SetImport(imp)
	next, err := Parse(mutate(t, func(d map[string]any) { d["version"] = "2099.01.01" }), "https://example/c.json")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Set(next) || !h.HasService("example-stream") || h.Base() != next {
		t.Error("the import was lost when the document changed")
	}
}

// TestRefresherAdGuard: the AdGuard refresher imports the document, keeps
// the last good copy across failures and restarts, and replicas pull it.
func TestRefresherAdGuard(t *testing.T) {
	srv := &docServer{}
	srv.set(http.StatusOK, adguardFixture)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	url := ts.URL + "/services.json"
	h := NewHolder(nil)
	r, err := NewRefresher(RefresherOptions{Kind: KindAdGuard, URL: url, Holder: h, Dir: dir, Logger: quiet(), RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if wait := r.Refresh(ctx); wait != time.Hour || !h.HasService("example-stream") || h.Import().Origin() != url {
		t.Fatalf("first refresh: wait %v, import %v", wait, h.Import())
	}
	if h.Base() != Embedded() {
		t.Error("the import replaced the catalogue document")
	}
	if st := r.Status(); st.Kind != KindAdGuard || st.LastError != "" {
		t.Errorf("status = %+v", st)
	}
	b, err := os.ReadFile(filepath.Join(dir, fileAdGuard)) //nolint:gosec // G304: the test's temp dir
	if err != nil || string(b) != adguardFixture {
		t.Errorf("kept copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileDocument)); err == nil {
		t.Error("the import was kept as the catalogue document")
	}
	// Failures keep the last good import.
	srv.set(http.StatusOK, `{"blocked_services": []}`)
	if wait := r.Refresh(ctx); wait != retryAfter || !h.HasService("example-stream") {
		t.Errorf("empty document: wait %v", wait)
	}
	if st := r.Status(); !strings.Contains(st.LastError, "no usable service") {
		t.Errorf("status = %+v", st)
	}
	srv.set(http.StatusBadGateway, "oops")
	r.Refresh(ctx)
	if !h.HasService("example-stream") {
		t.Error("an HTTP error dropped the import")
	}
	// A restart serves the kept copy before the first download.
	h2 := NewHolder(nil)
	r2, _ := NewRefresher(RefresherOptions{Kind: KindAdGuard, URL: url, Holder: h2, Dir: dir, Logger: quiet()})
	r2.loadPersisted()
	if !h2.HasService("example-stream") {
		t.Error("the kept import was not loaded")
	}
	// A kept copy of another URL is ignored.
	h3 := NewHolder(nil)
	r3, _ := NewRefresher(RefresherOptions{Kind: KindAdGuard, URL: "https://other.example/s.json", Holder: h3, Dir: dir, Logger: quiet()})
	r3.loadPersisted()
	if h3.Import() != nil {
		t.Error("a copy of another URL was loaded")
	}
	// A replica pulls the leader's copy.
	h4 := NewHolder(nil)
	r4, _ := NewRefresher(RefresherOptions{
		Kind: KindAdGuard, URL: url, Holder: h4, Logger: quiet(),
		Membership: &cluster.Static{SelfName: "b", LeaderName: "a", Members: []cluster.Peer{{Name: "a", Address: "10.0.0.1:7000"}, {Name: "b"}}},
		Pull: func(context.Context, string) ([]byte, string, error) {
			return []byte(adguardFixture), url, nil
		},
	})
	r4.Refresh(ctx)
	if !h4.HasService("example-tool") || h4.Import().Origin() != url {
		t.Error("the replica did not import the leader's copy")
	}
	if _, err := NewRefresher(RefresherOptions{Kind: "nope", URL: url, Holder: h}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}
