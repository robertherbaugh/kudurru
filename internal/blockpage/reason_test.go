package blockpage

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

func ev(at time.Time, client, name string, st pipeline.Status, list, rule string) *querylog.Event {
	return &querylog.Event{Time: at, Client: netip.MustParseAddr(client), Name: name, Status: st, List: list, Rule: rule, QType: 1, QClass: 1}
}

// TestQuerylogFinderTail: the newest matching block of the client and name
// within the window wins; other clients, names, statuses and old rows do
// not match.
func TestQuerylogFinderTail(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	tail := querylog.NewTail(64, 0, nil)
	for _, e := range []*querylog.Event{
		ev(now.Add(-11*time.Minute), "10.0.0.5", "old.example.", pipeline.StatusBlockedList, "default/old", "||old.example^"),
		ev(now.Add(-5*time.Minute), "10.0.0.5", "ads.example.", pipeline.StatusBlockedList, "default/first", "||ads.example^"),
		ev(now.Add(-4*time.Minute), "10.0.0.5", "ads.example.", pipeline.StatusBlockedRegex, "default/regex", "/ads/"),
		ev(now.Add(-3*time.Minute), "10.0.0.6", "ads.example.", pipeline.StatusBlockedList, "default/other-client", "x"),
		ev(now.Add(-2*time.Minute), "10.0.0.5", "ads.example.", pipeline.StatusBlockedRateLimit, "", ""),
		ev(now.Add(-time.Minute), "10.0.0.5", "www.example.", pipeline.StatusForwarded, "", ""),
		ev(now.Add(-time.Minute), "10.0.0.5", "tiktok.com.", pipeline.StatusBlockedList, "service:tiktok", "||tiktok.com^"),
		ev(now.Add(-time.Minute), "10.0.0.5", "www.google.com.", pipeline.StatusBlockedSafeSearch, "safesearch:google", ""),
		ev(now.Add(-time.Minute), "10.0.0.5", "cdn.example.", pipeline.StatusBlockedCNAME, "default/ads", "||x.cdn^"),
		{Time: now.Add(-time.Minute), Client: netip.MustParseAddr("10.0.0.5"), Name: "school.example.", Status: pipeline.StatusBlockedList, EDE: 17, HasEDE: true},
	} {
		tail.Publish(e)
	}
	f := &QuerylogFinder{Tail: tail}
	client := netip.MustParseAddr("10.0.0.5")
	for _, tc := range []struct {
		domain string
		kind   string
		list   string
		ok     bool
	}{
		{"ads.example", KindList, "default/regex", true},
		{"tiktok.com", KindService, "service:tiktok", true},
		{"www.google.com", KindSafeSearch, "safesearch:google", true},
		{"cdn.example", KindCNAME, "default/ads", true},
		{"school.example", KindPolicy, "", true},
		{"old.example", "", "", false},
		{"www.example", "", "", false},
	} {
		r, ok, note := f.Find(context.Background(), client, tc.domain, now)
		if ok != tc.ok || r.Kind != tc.kind || r.List != tc.list {
			t.Errorf("%s: %+v %v %q", tc.domain, r, ok, note)
		}
		if !ok && note != noteNotFound {
			t.Errorf("%s: note %q", tc.domain, note)
		}
	}
	// A v4-mapped client address matches the plain one.
	if r, ok, _ := f.Find(context.Background(), netip.MustParseAddr("::ffff:10.0.0.6"), "ads.example", now); !ok || r.List != "default/other-client" {
		t.Errorf("mapped client: %+v %v", r, ok)
	}
}

// TestQuerylogFinderPrivacy: anonymize_client matches the client's
// prefix (or its own pseudonym); hide_domains and stricter modes
// and a disabled query log give no reason, with a note saying why.
func TestQuerylogFinderPrivacy(t *testing.T) {
	now := time.Now()
	tail := querylog.NewTail(16, 0, nil)
	tail.Publish(ev(now.Add(-time.Minute), "10.0.0.0", "ads.example.", pipeline.StatusBlockedList, "default/ads", "||ads.example^"))
	pseudo := &querylog.Event{Time: now.Add(-time.Minute), ClientName: "anon-0123456789ab", Name: "pseudo.example.", Status: pipeline.StatusBlockedList, List: "default/p"}
	tail.Publish(pseudo)
	mode := func(m querylog.Mode, pseudonym bool) func() (querylog.Mode, bool) {
		return func() (querylog.Mode, bool) { return m, pseudonym }
	}
	client := netip.MustParseAddr("10.0.0.5")
	ctx := context.Background()

	if _, ok, _ := (&QuerylogFinder{Tail: tail}).Find(ctx, client, "ads.example", now); ok {
		t.Error("full: the /24 row is not this client")
	}
	if r, ok, _ := (&QuerylogFinder{Tail: tail, Privacy: mode(querylog.PrivacyAnonymizeClient, false)}).Find(ctx, client, "ads.example", now); !ok || r.List != "default/ads" {
		t.Errorf("anonymize_client prefix: %+v %v", r, ok)
	}
	pseudonyms := func(a netip.Addr) string {
		if a == client {
			return "anon-0123456789ab"
		}
		return "anon-ffffffffffff"
	}
	if r, ok, _ := (&QuerylogFinder{Tail: tail, Privacy: mode(querylog.PrivacyAnonymizeClient, true), Pseudonym: pseudonyms}).Find(ctx, client, "pseudo.example", now); !ok || r.List != "default/p" {
		t.Errorf("anonymize_client pseudonym: %+v %v", r, ok)
	}
	// Another device's pseudonymised block is not shown, and without the
	// pseudonym nothing is matched (the page would otherwise tell any
	// device which names others were blocked on).
	other := netip.MustParseAddr("10.0.0.6")
	if r, ok, _ := (&QuerylogFinder{Tail: tail, Privacy: mode(querylog.PrivacyAnonymizeClient, true), Pseudonym: pseudonyms}).Find(ctx, other, "pseudo.example", now); ok {
		t.Errorf("anonymize_client pseudonym, other device: %+v", r)
	}
	if r, ok, note := (&QuerylogFinder{Tail: tail, Privacy: mode(querylog.PrivacyAnonymizeClient, true)}).Find(ctx, client, "pseudo.example", now); ok || note != noteNotFound {
		t.Errorf("anonymize_client pseudonym without pseudonyms: %+v %v %q", r, ok, note)
	}
	for _, m := range []querylog.Mode{querylog.PrivacyHideDomains, querylog.PrivacyHideDomainsClients, querylog.PrivacyNothing} {
		if _, ok, note := (&QuerylogFinder{Tail: tail, Privacy: mode(m, false)}).Find(ctx, client, "ads.example", now); ok || note != notePrivacy {
			t.Errorf("%s: ok=%v note=%q", m, ok, note)
		}
	}
	if _, ok, note := (&QuerylogFinder{Disabled: true}).Find(ctx, client, "ads.example", now); ok || note != noteDisabled {
		t.Errorf("disabled: ok=%v note=%q", ok, note)
	}
}

// TestQuerylogFinderReader: rows already flushed to querylog.db (beyond
// the tail) are found through the reader.
func TestQuerylogFinderReader(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := sqlitelog.OptionsFrom(cfg.Querylog, t.TempDir(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w, err := sqlitelog.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	now := time.Now()
	batch := []*querylog.Event{
		ev(now.Add(-20*time.Minute), "10.0.0.5", "ads.example.", pipeline.StatusBlockedList, "default/too-old", "x"),
		ev(now.Add(-2*time.Minute), "10.0.0.5", "ads.example.", pipeline.StatusBlockedExact, "default/mine", "ads.example"),
		ev(now.Add(-time.Minute), "10.0.0.7", "ads.example.", pipeline.StatusBlockedList, "default/other", "x"),
	}
	for i, e := range batch {
		e.ID = uint64(i + 1)
	}
	if err := w.Write(batch); err != nil {
		t.Fatal(err)
	}
	f := &QuerylogFinder{Reader: w.Reader()}
	r, ok, _ := f.Find(ctx, netip.MustParseAddr("10.0.0.5"), "ads.example", now)
	if !ok || r.List != "default/mine" || r.Rule != "ads.example" || r.Kind != KindList || r.Status != "blocked_exact" {
		t.Errorf("reader: %+v %v", r, ok)
	}
	if _, ok, note := f.Find(ctx, netip.MustParseAddr("10.0.0.9"), "ads.example", now); ok || note != noteNotFound {
		t.Errorf("unknown client: %v %q", ok, note)
	}
}

func TestChain(t *testing.T) {
	ctx := context.Background()
	none := &fixedFinder{note: notePrivacy}
	hit := &fixedFinder{r: Reason{Kind: KindList, List: "default/ads"}, ok: true}
	if r, ok, _ := (Chain{none, hit}).Find(ctx, netip.Addr{}, "ads.example", time.Now()); !ok || r.List != "default/ads" {
		t.Errorf("chain: %+v %v", r, ok)
	}
	if _, ok, note := (Chain{none, &fixedFinder{}}).Find(ctx, netip.Addr{}, "ads.example", time.Now()); ok || note != notePrivacy {
		t.Errorf("chain note: %v %q", ok, note)
	}
	if _, _, note := (Chain{nil}).Find(ctx, netip.Addr{}, "ads.example", time.Now()); note != noteNoFinding {
		t.Errorf("empty chain note %q", note)
	}
}
