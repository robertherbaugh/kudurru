package main

import (
	"testing"

	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func TestDomainsOnLists(t *testing.T) {
	if n := domainsOnLists(nil); n != 0 {
		t.Fatalf("no snapshot: %d", n)
	}
	snap, reports := blocking.Rebuild(nil, []blocking.Spec{
		{Name: "default/ads", Mode: blocking.ModeBlock, Sources: []blocking.Source{
			blocking.LinesSource("inline", parser.FormatAdblock, []string{"||ads.example^", "||track.example^", "@@||ok.ads.example^"}),
		}},
		{Name: "default/hosts", Mode: blocking.ModeBlock, Sources: []blocking.Source{
			blocking.LinesSource("inline", parser.FormatHosts, []string{"0.0.0.0 a.example b.example ads.example"}),
		}},
		{Name: "default/allow", Mode: blocking.ModeAllow, Sources: []blocking.Source{
			blocking.LinesSource("inline", parser.FormatDomains, []string{"good.example", "fine.example"}),
		}},
	}, blocking.CompileOptions{})
	for _, r := range reports {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Name, r.Err)
		}
	}
	// 2 + 3 block entries; the @@ exception and the allow list do not count.
	if n := domainsOnLists(snap); n != 5 {
		t.Fatalf("domainsOnLists = %d, want 5 (snapshot entries %d)", n, snap.Entries())
	}
}
