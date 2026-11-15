package config

import (
	"strings"
	"testing"
)

func TestProviderResourcesHostBoundary(t *testing.T) {
	defaults, err := FromFlat(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	p := defaults.Auth.ProviderResources
	if len(p.AllowedOrigins) != 0 || len(p.LocalOrigins) != 0 || len(p.CAFiles) != 0 || !p.SystemTrust || len(p.Algorithms) != 1 || p.Algorithms[0] != "RS256" {
		t.Fatal("provider capabilities did not start with explicit safe defaults")
	}
	for _, suffix := range []string{"allowedOrigins", "localOrigins", "algorithms", "systemTrust", "caFiles"} {
		key, ok := Lookup("auth.providerResources." + suffix)
		if !ok || !key.HostOnly || key.Scope != ScopeNode || !key.RestartRequired || PublicClusterKey(key) {
			t.Fatal("provider resource capability became public or reloadable", suffix)
		}
	}
	if _, _, err := ApplyClusterDocument(defaults, map[string]any{"auth": map[string]any{"providerResources": map[string]any{"allowedOrigins": []string{"https://injected.example"}}}}); err == nil {
		t.Fatal("cluster settings delegated provider network authority")
	}
	valid := p
	valid.AllowedOrigins = []string{"https://login.example", "https://[::1]:8443", "http://127.0.0.1:5556"}
	valid.LocalOrigins = []string{"https://[::1]:8443", "http://127.0.0.1:5556"}
	valid.CAFiles = []string{"/etc/kudurru/provider-ca.pem"}
	if errs := valid.validate(); len(errs) != 0 {
		t.Fatal(errs)
	}
	for name, change := range map[string]func(*ProviderResources){
		"userinfo":  func(v *ProviderResources) { v.AllowedOrigins = []string{"https://user:password@login.example"} },
		"path":      func(v *ProviderResources) { v.AllowedOrigins = []string{"https://login.example/"} },
		"query":     func(v *ProviderResources) { v.AllowedOrigins = []string{"https://login.example?"} },
		"fragment":  func(v *ProviderResources) { v.AllowedOrigins = []string{"https://login.example#"} },
		"uppercase": func(v *ProviderResources) { v.AllowedOrigins = []string{"https://LOGIN.example"} },
		"duplicate": func(v *ProviderResources) {
			v.AllowedOrigins = []string{"https://login.example", "https://login.example"}
		},
		"port":                 func(v *ProviderResources) { v.AllowedOrigins = []string{"https://login.example:65536"} },
		"padded port":          func(v *ProviderResources) { v.AllowedOrigins = []string{"https://login.example:0443"} },
		"unbounded":            func(v *ProviderResources) { v.AllowedOrigins = []string{"https://" + strings.Repeat("x", 2048)} },
		"count":                func(v *ProviderResources) { v.AllowedOrigins = make([]string, 129) },
		"local delegation":     func(v *ProviderResources) { v.LocalOrigins = []string{"https://local.example"} },
		"http":                 func(v *ProviderResources) { v.AllowedOrigins = []string{"http://login.example"} },
		"symmetric":            func(v *ProviderResources) { v.Algorithms = []string{"HS256"} },
		"missing algorithms":   func(v *ProviderResources) { v.Algorithms = nil },
		"algorithm duplicates": func(v *ProviderResources) { v.Algorithms = []string{"RS256", "RS256"} },
		"relative CA":          func(v *ProviderResources) { v.CAFiles = []string{"certs/ca.pem"} },
		"unclean CA":           func(v *ProviderResources) { v.CAFiles = []string{"/etc/../ca.pem"} },
		"missing trust": func(v *ProviderResources) {
			v.AllowedOrigins = []string{"https://login.example"}
			v.SystemTrust = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := p
			change(&value)
			if len(value.validate()) == 0 {
				t.Fatal("invalid provider authority accepted")
			}
		})
	}
}
