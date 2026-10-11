package deploy_test

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	yamlv3 "go.yaml.in/yaml/v3"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// reservedEnv are the KUDURRU_* variables without "__" that the binary (or
// a package landing in M3) reads directly; config.Load ignores them.
var reservedEnv = map[string]string{ //nolint:gosec // variable names, not credentials
	"KUDURRU_CONFIG":                       "cmd/kudurru/main.go: path of kudurru.yaml",
	"KUDURRU_HEADLESS_SERVICE":             "cmd/kudurru/serve.go: <pod>.<headless> for the leader's internal listener",
	"KUDURRU_ENCRYPTION_KEY_FILE":          "cmd/kudurru/serve.go readEncryptionKeys",
	"KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE": "cmd/kudurru/serve.go readEncryptionKeys",
	"KUDURRU_ADMIN_PASSWORD_FILE":          "§8.7 first-start admin (reserved in internal/config)",
	"KUDURRU_BOOTSTRAP_TOKEN_FILE":         "§8.7 bootstrap service-account token",
}

// pendingKeys are settings keys the deployment assets already render whose
// metadata row lands with another M3 package. The strict key check accepts
// them; config.Load ignores unknown file keys. Each has a docs/followups.md row.
var pendingKeys = map[string]string{
	"externalDNS.webhook.listen": "external-dns webhook listener (API package, §5.6)",
}

// checkEnvName fails for a KUDURRU_* variable that neither maps onto a
// settings key (KUDURRU_<SECTION>__<KEY>[_FILE]) nor is reserved; the binary
// refuses to start on an unknown "__" variable.
func checkEnvName(name string) error {
	if !strings.HasPrefix(name, config.EnvPrefix) {
		return nil
	}
	if _, ok := reservedEnv[name]; ok {
		return nil
	}
	if !strings.Contains(name, "__") {
		return fmt.Errorf("%s is neither a settings key (KUDURRU_<SECTION>__<KEY>) nor a reserved variable", name)
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, config.EnvPrefix), "_FILE")
	lower := strings.ToLower(strings.ReplaceAll(rest, "__", "."))
	for _, k := range config.Schema {
		if strings.ToLower(k.Path) == lower {
			if k.Type == config.TypeList || k.Type == config.TypeMap {
				return fmt.Errorf("%s: %s is a %s key and cannot be set from the environment", name, k.Path, k.Type)
			}
			return nil
		}
	}
	return fmt.Errorf("%s does not match a settings key (kudurru config schema)", name)
}

// unknownConfigKeys returns the leaf paths of a kudurru.yaml document that
// no settings key covers. Keys of type list or map cover everything below.
func unknownConfigKeys(doc map[string]any) []string {
	var out []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if prefix != "" {
			if k, ok := config.Lookup(prefix); ok {
				if k.Type != config.TypeList && k.Type != config.TypeMap {
					if _, isMap := v.(map[string]any); isMap {
						out = append(out, prefix+" (expected a "+string(k.Type)+")")
					}
				}
				return
			}
			if _, ok := pendingKeys[prefix]; ok {
				return
			}
		}
		m, ok := v.(map[string]any)
		if !ok {
			out = append(out, prefix)
			return
		}
		for key, child := range m {
			p := key
			if prefix != "" {
				p = prefix + "." + key
			}
			walk(p, child)
		}
	}
	walk("", doc)
	sort.Strings(out)
	return out
}

// checkConfig loads a rendered kudurru.yaml through config.Load with the
// given environment (exactly what `kudurru serve --config` does) and fails
// on unknown keys or a validation error.
func checkConfig(t *testing.T, label string, body []byte, env []string) *config.Config {
	t.Helper()
	var doc map[string]any
	if err := yamlv3.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%s: kudurru.yaml does not parse: %v", label, err)
	}
	if unknown := unknownConfigKeys(doc); len(unknown) > 0 {
		t.Errorf("%s: kudurru.yaml has keys the binary does not know: %v", label, unknown)
	}
	f, err := os.CreateTemp(t.TempDir(), "kudurru-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if err := checkEnvName(name); err != nil {
			t.Errorf("%s: %v", label, err)
		}
	}
	cfg, err := config.Load(f.Name(), env)
	if err != nil {
		t.Fatalf("%s: config.Load rejects the rendered kudurru.yaml: %v\n%s", label, err, body)
	}
	return cfg
}
