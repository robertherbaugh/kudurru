package main

import (
	"strings"
	"testing"
)

// D397: `kudurru inventory` walks every page of GET /inventory and passes
// its filters through unchanged.
func TestInventoryCommand(t *testing.T) {
	h := loggedIn(t)
	golden(t, h, "inventory", h.mustRun("inventory"))
	golden(t, h, "inventory-wide", h.mustRun("inventory", "-o", "wide"))
	h.mustRun("inventory", "--kind", "dhcp", "--stale", "false", "--search", "02:00:5e", "--source", "dhcp/lan")
	got := h.api.requested("GET /api/v1/inventory")
	last := got[len(got)-2]
	for _, want := range []string{"kind=dhcp", "stale=false", "q=02%3A00%3A5e", "source=dhcp%2Flan", "limit=1000"} {
		if !strings.Contains(last, want) {
			t.Fatalf("inventory request %q lacks %s", last, want)
		}
	}
	if out := h.mustRun("inventory", "--limit", "1", "-o", "json"); strings.Count(out, `"source"`) != 1 {
		t.Fatalf("--limit 1 printed %s", out)
	}
	if _, _, err := h.run("inventory", "--kind", "other"); err == nil {
		t.Fatal("invalid --kind accepted")
	}
}
