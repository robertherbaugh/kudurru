package config

import (
	"strings"
	"testing"
	"time"
)

func TestListenerLimitsConfig(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Listeners.Limits; got.MaxTCPQueries != 0 || got.QUICMaxConns != 0 || got.IdleTimeout.Std() != 10*time.Second || got.QUICIdleTimeout.Std() != 30*time.Second {
		t.Fatalf("defaults: %+v", got)
	}
	for _, tc := range []struct {
		key, value string
		valid      bool
	}{
		{"maxTCPQueries", "0", true}, {"maxTCPQueries", "8", true}, {"maxTCPQueries", "-1", false}, {"maxTCPQueries", "2147483648", false},
		{"quicMaxConns", "0", true}, {"quicMaxConns", "2", true}, {"quicMaxConns", "-1", false}, {"quicMaxConns", "2147483648", false},
		{"idleTimeout", "0s", true}, {"idleTimeout", "250ms", true}, {"idleTimeout", "-1s", false}, {"idleTimeout", "bad", false},
		{"quicIdleTimeout", "0s", true}, {"quicIdleTimeout", "1s", true}, {"quicIdleTimeout", "-1s", false}, {"quicIdleTimeout", "bad", false},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			_, err := Load("", []string{"KUDURRU_LISTENERS__LIMITS__" + strings.ToUpper(tc.key) + "=" + tc.value})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t error=%v", tc.valid, err)
			}
		})
	}
	for _, key := range []string{"maxTCPQueries", "quicMaxConns", "idleTimeout", "quicIdleTimeout"} {
		found := false
		for _, meta := range Schema {
			if meta.Path == "listeners.limits."+key {
				found = true
				if !meta.HostOnly || !meta.RestartRequired || meta.Scope != ScopeNode {
					t.Fatalf("%s lacks host/restart/node scope", key)
				}
			}
		}
		if !found {
			t.Fatalf("no metadata for %s", key)
		}
	}
}
