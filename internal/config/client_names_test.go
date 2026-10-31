package config

import (
	"strings"
	"testing"
	"time"
)

func TestReversePTRHostConfiguration(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientNames.ReversePTR.Enabled || cfg.ClientNames.ReversePTR.Timeout.Std() != time.Second || cfg.ClientNames.ReversePTR.MaxEntries != 4096 {
		t.Fatal("unsafe defaults")
	}
	env := []string{"KUDURRU_CLIENTNAMES__REVERSEPTR__ENABLED=true", "KUDURRU_CLIENTNAMES__REVERSEPTR__SERVERS=127.0.0.1:5300, [::1]:5300", "KUDURRU_CLIENTNAMES__REVERSEPTR__NETWORKS=192.0.2.0/24, 2001:db8::/32"}
	cfg, err = Load("", env)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ClientNames.ReversePTR.Enabled || len(cfg.ClientNames.ReversePTR.Servers) != 2 || len(cfg.ClientNames.ReversePTR.Networks) != 2 {
		t.Fatal("typed environment lost")
	}
	for _, invalid := range []string{"TIMEOUT=0s", "MAXAGE=0s", "NEGATIVETTL=0s", "WORKERS=9", "MAXENTRIES=65537", "SERVERS=resolver.local:53", "NETWORKS=192.0.2.1"} {
		values := append(append([]string(nil), env...), "KUDURRU_CLIENTNAMES__REVERSEPTR__"+invalid)
		if _, err := Load("", values); err == nil {
			t.Fatalf("invalid host value accepted: %s", invalid)
		}
	}
	if _, err := Load("", env[:1]); err == nil {
		t.Fatal("enabled implicit network accepted")
	}
	for _, key := range Schema {
		if strings.HasPrefix(key.Path, "clientNames.") && (!key.HostOnly || !key.RestartRequired || key.Scope != ScopeNode || key.Group != GroupClientNames || key.Help == "") {
			t.Fatal("host authority missing", key.Path)
		}
	}
}
