package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTSIGKeysAndNotifySource(t *testing.T) {
	for _, path := range []string{"tsigKeys", "notify.source"} {
		k, ok := Lookup(path)
		if !ok || !k.HostOnly || !k.RestartRequired {
			t.Fatalf("%s: %+v", path, k)
		}
	}
	if k, _ := Lookup("tsigKeys"); !k.WriteOnly {
		t.Fatal("tsigKeys must be writeOnly")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
tsigKeys:
  - { name: transfer-key., algorithm: hmac-sha256, secretFile: /run/secrets/transfer }
  - { name: Backup.Example, algorithm: HMAC-SHA512, secretFile: /run/secrets/backup }
notify:
  source: 192.0.2.53
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TSIGKeys) != 2 || cfg.TSIGKeys[1].Name != "Backup.Example" || cfg.TSIGKeys[0].SecretFile != "/run/secrets/transfer" || cfg.Notify.Source != "192.0.2.53" {
		t.Fatalf("decoded %+v %+v", cfg.TSIGKeys, cfg.Notify)
	}
	if def, err := Load("", nil); err != nil || len(def.TSIGKeys) != 0 || def.Notify.Source != "" {
		t.Fatalf("defaults: %+v %v", def, err)
	}
	cfg, err = Load("", []string{"KUDURRU_NOTIFY__SOURCE=2001:db8::53"})
	if err != nil || cfg.Notify.Source != "2001:db8::53" {
		t.Fatalf("env: %v %v", cfg, err)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte(`
tsigKeys:
  - { name: "", algorithm: hmac-sha256, secretFile: /a }
  - { name: k1, algorithm: hmac-md5, secretFile: /b }
  - { name: k2, algorithm: hmac-sha256 }
  - { name: K1., algorithm: hmac-sha512, secretFile: /c }
notify:
  source: ns1.example.org
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(bad, nil)
	if err == nil {
		t.Fatal("invalid tsigKeys must be rejected")
	}
	for _, want := range []string{"tsigKeys[0].name is required", "tsigKeys[1].algorithm", "tsigKeys[2].secretFile is required", "tsigKeys[3]: key \"K1.\" is defined twice", "notify.source"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}
