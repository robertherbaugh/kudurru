package config

import (
	"strings"
	"testing"
)

func TestExpectedKubeClusterIdentityConfig(t *testing.T) {
	const key = "store.kube.expectedClusterID"
	schema, ok := Lookup(key)
	if !ok || !schema.HostOnly || !schema.RestartRequired || schema.Scope != ScopeNode {
		t.Fatal("cluster pin must be host-only and restart-required")
	}
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"empty online compatibility", "", true},
		{"namespace UID", "ab123456-1234-5678-abcd-123456789012", true},
		{"whitespace", " invalid ", false},
		{"control", "bad\nidentity", false},
		{"oversized", strings.Repeat("x", 129), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load("", []string{"KUDURRU_STORE__KUBE__EXPECTEDCLUSTERID=" + tc.value})
			if (err == nil) != tc.valid {
				t.Fatalf("configuration admission: accepted=%t want=%t", err == nil, tc.valid)
			}
			if err == nil && cfg.Store.Kube.ExpectedClusterID != tc.value {
				t.Fatal("host environment pin was not loaded")
			}
		})
	}
}
