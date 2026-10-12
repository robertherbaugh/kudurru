package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBlockingAndInternalDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Blocking
	if b.RefreshInterval.Std() != 24*time.Hour || b.InitStrategy != "fast" || !b.AllowDegraded || b.ListsDir != "" {
		t.Fatalf("blocking defaults = %+v", b)
	}
	if got := cfg.BlockingListsDir(); got != filepath.Join("/var/lib/kudurru", "lists") {
		t.Errorf("BlockingListsDir = %q", got)
	}
	in := cfg.Listeners.Internal
	if in.Address != "0.0.0.0:8082" || in.TokenFile != "" || in.TLS != (InternalTLS{}) {
		t.Errorf("internal listener defaults = %+v", in)
	}
	for _, path := range []string{"listeners.internal.address", "listeners.internal.tokenFile", "listeners.internal.tls.certFile", "blocking.listsDir"} {
		if k, _ := Lookup(path); !k.HostOnly {
			t.Errorf("%s must be hostOnly", path)
		}
	}
}

func TestBlockingFileEnvAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
node: { dataDir: /data }
blocking: { refreshInterval: 6h, listsDir: /cache/lists }
listeners:
  internal: { address: "10.0.0.11:8082", tokenFile: /run/secrets/internal_token }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, []string{"KUDURRU_BLOCKING__INITSTRATEGY=failOnError", "KUDURRU_BLOCKING__ALLOWDEGRADED=false"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Blocking.RefreshInterval.Std() != 6*time.Hour || cfg.Blocking.InitStrategy != "failOnError" || cfg.Blocking.AllowDegraded {
		t.Errorf("blocking = %+v", cfg.Blocking)
	}
	if cfg.BlockingListsDir() != "/cache/lists" || cfg.Listeners.Internal.Address != "10.0.0.11:8082" || cfg.Listeners.Internal.TokenFile != "/run/secrets/internal_token" {
		t.Errorf("config = %+v %+v", cfg.Blocking, cfg.Listeners.Internal)
	}

	for env, want := range map[string]string{
		"KUDURRU_BLOCKING__INITSTRATEGY=eventually":                 "blocking.initStrategy",
		"KUDURRU_BLOCKING__REFRESHINTERVAL=10s":                     "blocking.refreshInterval",
		"KUDURRU_LISTENERS__INTERNAL__TLS__CERTFILE=/certs/tls.crt": "certFile and keyFile",
	} {
		_, err := Load("", []string{env})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", env, err, want)
		}
	}
}
