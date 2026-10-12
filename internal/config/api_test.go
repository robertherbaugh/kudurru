package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIListenerDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	api := cfg.Listeners.API
	if api.Address != "0.0.0.0:8080" || api.TLS.Auto != APITLSSelfSigned || !api.TLS.Enabled() || api.PathPrefix != "" {
		t.Fatalf("api listener defaults %+v", api)
	}
	w := cfg.ExternalDNS.Webhook
	if w.Enabled || w.Listen != "127.0.0.1:8888" || w.Namespace != "external-dns" {
		t.Fatalf("external-dns defaults %+v", w)
	}
}

func TestAPIListenerValidation(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"tls mode", "listeners:\n  api: {tls: {auto: acme}}\n", "listeners.api.tls.auto"},
		{"cert without key", "listeners:\n  api: {tls: {certFile: /c.pem}}\n", "certFile and keyFile"},
		{"prefix slash", "listeners:\n  api: {pathPrefix: kudurru/}\n", "listeners.api.pathPrefix"},
		{"trusted proxy", "listeners:\n  api: {trustedProxies: [nope]}\n", "listeners.api.trustedProxies"},
		{"webhook namespace", "externalDNS:\n  webhook: {enabled: true, namespace: \"\"}\n", "externalDNS.webhook.namespace"},
	}
	for _, tc := range cases {
		path := filepath.Join(t.TempDir(), "kudurru.yaml")
		if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %s", tc.name, err, tc.want)
		}
	}
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	ok := "listeners:\n  api: {pathPrefix: /kudurru, tls: {auto: \"off\"}, trustedProxies: [10.0.0.0/8, 192.0.2.1]}\nexternalDNS:\n  webhook: {enabled: true, zoneSelector: {team: web}}\n"
	if err := os.WriteFile(path, []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listeners.API.TLS.Enabled() || cfg.ExternalDNS.Webhook.ZoneSelector["team"] != "web" || len(cfg.Listeners.API.TrustedProxies) != 2 {
		t.Fatalf("parsed %+v %+v", cfg.Listeners.API, cfg.ExternalDNS)
	}
}

func TestParsePrefixOrAddr(t *testing.T) {
	for in, want := range map[string]string{"10.0.0.1": "10.0.0.1/32", "10.1.2.3/8": "10.0.0.0/8", "::ffff:10.0.0.1": "10.0.0.1/32", " 2001:db8::1 ": "2001:db8::1/128"} {
		p, err := ParsePrefixOrAddr(in)
		if err != nil || p.String() != want {
			t.Errorf("%q: %v %v, want %s", in, p, err, want)
		}
	}
	if _, err := ParsePrefixOrAddr("example.com"); err == nil {
		t.Error("hostname accepted")
	}
}
