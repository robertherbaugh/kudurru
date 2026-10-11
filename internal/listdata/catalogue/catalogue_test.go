package catalogue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/blocking"
)

// TestEmbedded validates the embedded document the way the refresher
// validates a download, and checks the promises the API and the docs make
// about it.
func TestEmbedded(t *testing.T) {
	c, err := Parse(embeddedJSON, OriginEmbedded)
	if err != nil {
		t.Fatal(err)
	}
	if c.SchemaVersion != SchemaVersion || c.License != "Apache-2.0" || c.Origin() != OriginEmbedded {
		t.Errorf("header = %d %q %q", c.SchemaVersion, c.License, c.Origin())
	}
	if !regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}$`).MatchString(c.Version) {
		t.Errorf("version %q is not YYYY.MM.DD", c.Version)
	}
	if n := len(c.Services); n < 25 || n > 60 {
		t.Errorf("%d services; the v0.1 catalogue carries the common 25-60", n)
	}
	for _, id := range []string{"youtube", "tiktok", "facebook", "instagram", "whatsapp", "snapchat", "twitter", "reddit", "discord",
		"twitch", "netflix", "steam", "epicgames", "roblox", "minecraft", "fortnite", "pinterest", "tumblr", "telegram", "signal",
		"spotify", "primevideo", "disneyplus", "hulu", "9gag", "linkedin"} {
		if !c.HasService(id) {
			t.Errorf("service %q missing", id)
		}
	}
	for _, id := range []string{"google", "youtube", "bing", "duckduckgo", "yandex", "pixabay", "brave"} {
		found := false
		for _, e := range c.SafeSearch {
			found = found || e.ID == id
		}
		if !found {
			t.Errorf("safe-search engine %q missing", id)
		}
	}
	// Every service compiles to a block list with one entry per rule.
	snap, reports := blocking.Rebuild(nil, c.Specs(), blocking.CompileOptions{})
	for _, r := range reports {
		s, _ := c.Service(strings.TrimPrefix(r.Name, ServiceListPrefix))
		if r.Err != nil || r.Stats.Entries != len(s.Rules) || r.Stats.Invalid != 0 {
			t.Errorf("%s: err %v, %d entries for %d rules, %d invalid", r.Name, r.Err, r.Stats.Entries, len(s.Rules), r.Stats.Invalid)
		}
	}
	if got := snap.Contains("www.youtube.com"); got[ListName("youtube")] != "||youtube.com^" {
		t.Errorf("www.youtube.com: %v", got)
	}
	// No service list blocks a safe-search target of another vendor, so
	// blocking one service never breaks safe search on another engine.
	// (restrict.youtube.com is under ||youtube.com^: blocking YouTube
	// blocks its restricted mode too, as it should.)
	for _, e := range c.SafeSearch {
		for _, tgt := range []string{e.Target, e.ModerateTarget} {
			if tgt == "" {
				continue
			}
			if hits := snap.Contains(tgt); len(hits) > 0 && e.ID != "youtube" {
				t.Errorf("safe-search target %s is blocked by %v", tgt, hits)
			}
		}
	}
}

// TestProvenance is the licences.yml-style check for the catalogue: a
// permissive licence line, the licence text, the digest of the embedded
// bytes, and one row per entry naming the same source as the document.
func TestProvenance(t *testing.T) {
	prov := Provenance()
	m := regexp.MustCompile(`(?m)^SPDX-License-Identifier: (\S+)$`).FindStringSubmatch(prov)
	if m == nil || m[1] != "Apache-2.0" {
		t.Fatalf("PROVENANCE.md licence line = %v, want Apache-2.0", m)
	}
	if !strings.Contains(License(), "Apache License") || !strings.Contains(License(), "Version 2.0, January 2004") {
		t.Error("LICENSE is not the Apache-2.0 text")
	}
	sum := sha256.Sum256(embeddedJSON)
	if want := hex.EncodeToString(sum[:]); !strings.Contains(prov, "| SHA-256 | `"+want+"` |") {
		t.Errorf("provenance does not record the embedded catalogue's SHA-256 %s", want)
	}
	c := Embedded()
	if !strings.Contains(prov, "| Catalogue version | `"+c.Version+"` |") {
		t.Errorf("provenance does not record version %s", c.Version)
	}
	for _, s := range c.Services {
		row := "| service | " + s.ID + " | " + s.Name + " | "
		i := strings.Index(prov, row)
		if i < 0 {
			t.Errorf("provenance has no row for service %s", s.ID)
			continue
		}
		if line := prov[i : strings.IndexByte(prov[i:], '\n')+i]; !strings.HasSuffix(line, " | "+s.Source+" |") {
			t.Errorf("provenance row %q does not name the source %q", line, s.Source)
		}
	}
	for _, e := range c.SafeSearch {
		row := "| safesearch | " + e.ID + " | " + e.Name + " | "
		i := strings.Index(prov, row)
		if i < 0 {
			t.Errorf("provenance has no row for engine %s", e.ID)
			continue
		}
		if line := prov[i : strings.IndexByte(prov[i:], '\n')+i]; !strings.HasSuffix(line, " | "+e.Source+" |") {
			t.Errorf("provenance row %q does not name the source %q", line, e.Source)
		}
	}
	// Licence hygiene: no entry may cite a copyleft table.
	for _, banned := range []string{"AdguardTeam", "adguard.com", "HostlistsRegistry", "hagezi"} {
		if strings.Contains(string(embeddedJSON), banned) {
			t.Errorf("catalogue.json mentions %s", banned)
		}
	}
}

func TestSafeSearch(t *testing.T) {
	c := Embedded()
	cases := []struct {
		q, mode, engine, target string
	}{
		{"www.google.com.", "", "google", "forcesafesearch.google.com"},
		{"WWW.Google.Co.UK", ModeStrict, "google", "forcesafesearch.google.com"},
		{"www.google.cat", "", "google", "forcesafesearch.google.com"},
		{"www.youtube.com.", ModeStrict, "youtube", "restrict.youtube.com"},
		{"m.youtube.com.", ModeModerate, "youtube", "restrictmoderate.youtube.com"},
		{"www.bing.com.", ModeModerate, "bing", "strict.bing.com"},
		{"duckduckgo.com.", "", "duckduckgo", "safe.duckduckgo.com"},
		{"search.brave.com.", "", "brave", "forcesafe.search.brave.com"},
		{"pixabay.com.", "", "pixabay", "safesearch.pixabay.com"},
		{"yandex.ru.", "", "yandex", ""},
	}
	for _, tc := range cases {
		r, ok := c.RewriteFor(tc.q, tc.mode)
		if !ok || r.Engine != tc.engine || r.Target != tc.target {
			t.Errorf("RewriteFor(%q, %q) = %+v, %v", tc.q, tc.mode, r, ok)
		}
	}
	if r, _ := c.RewriteFor("yandex.ru", ""); len(r.Addrs) != 1 || r.Addrs[0].String() != "213.180.193.56" || r.List() != "safesearch:yandex" {
		t.Errorf("yandex = %+v", r)
	}
	for _, q := range []string{"google.com.", "mail.google.com.", "youtube.com.", "forcesafesearch.google.com.", "bing.com.", "example.com."} {
		if r, ok := c.RewriteFor(q, ""); ok {
			t.Errorf("RewriteFor(%q) = %+v; only the documented host names are rewritten", q, r)
		}
	}
	if n := testing.AllocsPerRun(100, func() { c.RewriteFor("www.google.de.", "") }); n != 0 {
		t.Errorf("RewriteFor allocates %v times", n)
	}
}

func mutate(t *testing.T, edit func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(embeddedJSON, &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseRejects(t *testing.T) {
	svc := func(doc map[string]any) map[string]any { return doc["services"].([]any)[0].(map[string]any) }
	eng := func(doc map[string]any) map[string]any { return doc["safeSearch"].([]any)[0].(map[string]any) }
	cases := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"schema 2", func(d map[string]any) { d["schemaVersion"] = 2 }, "unsupported schemaVersion 2"},
		{"no schema", func(d map[string]any) { delete(d, "schemaVersion") }, "no schemaVersion"},
		{"no version", func(d map[string]any) { d["version"] = "" }, "version is empty"},
		{"bad id", func(d map[string]any) { svc(d)["id"] = "You Tube" }, "id must match"},
		{"duplicate id", func(d map[string]any) {
			s := d["services"].([]any)
			s[1].(map[string]any)["id"] = s[0].(map[string]any)["id"]
		}, "duplicate id"},
		{"no rules", func(d map[string]any) { svc(d)["rules"] = []any{} }, "a service needs 1 to"},
		{"exception rule", func(d map[string]any) { svc(d)["rules"] = []any{"@@||example.com^"} }, "exception"},
		{"comment rule", func(d map[string]any) { svc(d)["rules"] = []any{"! comment", "||example.com^"} }, "lines are rules"},
		{"cosmetic rule", func(d map[string]any) { svc(d)["rules"] = []any{"example.com##.ad"} }, "invalid rule"},
		{"modifier rule", func(d map[string]any) { svc(d)["rules"] = []any{"||example.com^$important"} }, "unsupported rule"},
		{"newline", func(d map[string]any) { svc(d)["rules"] = []any{"||a.example^\n||b.example^"} }, "line break"},
		{"no source", func(d map[string]any) { svc(d)["source"] = " " }, "source is empty"},
		{"bad category", func(d map[string]any) { svc(d)["category"] = "Video" }, "category"},
		{"no target", func(d map[string]any) { delete(eng(d), "target"); delete(eng(d), "addresses") }, "needs a target or addresses"},
		{"both", func(d map[string]any) { eng(d)["addresses"] = []any{"192.0.2.1"} }, "both target and addresses"},
		{"bad target", func(d map[string]any) { eng(d)["target"] = "Safe.Example." }, "not a lower-case host name"},
		{"bad address", func(d map[string]any) {
			e := eng(d)
			delete(e, "target")
			delete(e, "moderateTarget")
			e["addresses"] = []any{"0.0.0.0"}
		}, "not a unicast"},
		{"duplicate domain", func(d map[string]any) {
			s := d["safeSearch"].([]any)
			s[1].(map[string]any)["domains"] = s[0].(map[string]any)["domains"]
		}, "already rewritten"},
		{"self target", func(d map[string]any) { e := eng(d); e["domains"] = []any{e["target"]} }, "its own target"},
		{"not json", nil, "catalogue:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := []byte("{not json")
			if tc.edit != nil {
				b = mutate(t, tc.edit)
			}
			_, err := Parse(b, "test")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := Parse(mutate(t, func(d map[string]any) { d["schemaVersion"] = 9 }), "x"); !errors.Is(err, ErrSchemaVersion) {
		t.Errorf("err = %v, want ErrSchemaVersion", err)
	}
	if _, err := Parse(make([]byte, MaxBytes+1), "x"); err == nil {
		t.Error("an oversized document was accepted")
	}
	// Unknown fields are ignored within a schema version.
	c, err := Parse(mutate(t, func(d map[string]any) { d["future"] = true; svc(d)["logo"] = "x.svg" }), "https://example/c.json")
	if err != nil || c.Origin() != "https://example/c.json" {
		t.Fatalf("additive fields: %v", err)
	}
}

func TestHolder(t *testing.T) {
	h := NewHolder(nil)
	if h.Current() != Embedded() || !h.HasService("youtube") {
		t.Fatal("a new holder serves the embedded catalogue")
	}
	ch := h.Subscribe()
	if h.Set(Embedded()) {
		t.Error("Set with the same digest reported a change")
	}
	next, err := Parse(mutate(t, func(d map[string]any) {
		d["version"] = "2099.01.01"
		d["services"] = d["services"].([]any)[:1]
	}), "https://example/c.json")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Set(next) || h.Current() != next {
		t.Fatal("Set did not replace the catalogue")
	}
	select {
	case <-ch:
	default:
		t.Error("subscriber not notified")
	}
	specs, fp := h.Lists()
	if len(specs) != 1 || fp != "catalogue:"+next.Digest() {
		t.Errorf("Lists = %d specs, %q", len(specs), fp)
	}
	var nilHolder *Holder
	if nilHolder.Current() != Embedded() {
		t.Error("a nil holder serves the embedded catalogue")
	}
}
