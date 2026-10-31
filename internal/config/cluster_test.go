package config

import (
	"slices"
	"testing"
)

func TestClusterDocumentReplacesPublicSettingsAndKeepsHostLocks(t *testing.T) {
	host, err := Load("", []string{"KUDURRU_CACHE__MAXENTRIES=789", "KUDURRU_NODE__NAME=member-a"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ApplyClusterDocument(host, map[string]any{"cache": map[string]any{"maxEntries": 456, "enabled": true}, "response": map[string]any{"filterAAAA": true}})
	// cache.enabled is deliberately unknown; maxEntries=0 is the existing switch.
	if err == nil {
		t.Fatal("unknown settings key accepted")
	}
	next, ignored, err := ApplyClusterDocument(host, map[string]any{"cache": map[string]any{"maxEntries": 456}, "response": map[string]any{"filterAAAA": true}})
	if err != nil {
		t.Fatal(err)
	}
	if next.Cache.MaxEntries != 789 || next.Node.Name != "member-a" || !next.Response.FilterAAAA || !slices.Equal(ignored, []string{"cache.maxEntries"}) {
		t.Fatalf("host override lost or canonical setting missing: ignored=%v", ignored)
	}
	reset, _, err := ApplyClusterDocument(next, nil)
	if err != nil || reset.Response.FilterAAAA || reset.Cache.MaxEntries != 789 {
		t.Fatal("deletion did not reset public defaults while retaining local lock", err)
	}
	if host.Response.FilterAAAA {
		t.Fatal("preparation mutated host input")
	}
	seed := ClusterSeed(host)
	if seed["cache"].(map[string]any)["maxEntries"] == 789 {
		t.Fatal("seed distributed local environment override")
	}
}

func TestClusterDocumentRejectsPrivilegedOrMalformedKeys(t *testing.T) {
	host, err := Load("", []string{"KUDURRU_CACHE__MAXENTRIES=789"})
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []map[string]any{
		{"listeners": map[string]any{"api": map[string]any{"address": "127.0.0.1:9999"}}},
		{"auth": map[string]any{"identityProviders": []any{}}},
		{"audit": map[string]any{"store": "local"}},
		{"cache.maxEntries": 5},
		{"cache": map[string]any{"maxEntries": "bad"}},
		{"node": map[string]any{"name": "overridden"}},
	} {
		if _, _, err := ApplyClusterDocument(host, document); err == nil {
			t.Fatal("rejected key was accepted")
		}
	}
}
