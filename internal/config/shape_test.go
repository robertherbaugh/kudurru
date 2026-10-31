package config

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// validateWith decodes the defaults with path set to v, like the file
// loader and PATCH /settings do, and validates the result.
func validateWith(t *testing.T, path string, v any) error {
	t.Helper()
	flat := Defaults()
	flat[path] = v
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		t.Fatalf("%s=%v: %v", path, v, err)
	}
	return cfg.Validate()
}

// jsonForm is v as the validator sees it (decoded JSON).
func jsonForm(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// walkShapes calls fn for every shape nested in s (s included).
func walkShapes(path string, s *Shape, fn func(string, *Shape)) {
	if s == nil {
		return
	}
	fn(path, s)
	walkShapes(path+"[]", s.Items, fn)
	walkShapes(path+"{key}", s.Keys, fn)
	walkShapes(path+"{}", s.Values, fn)
	for i := range s.Fields {
		walkShapes(path+"."+s.Fields[i].Name, &s.Fields[i].Shape, fn)
	}
}

func TestSchemaMetadataComplete(t *testing.T) {
	for _, k := range Schema {
		if k.Label == "" {
			t.Errorf("%s: no label", k.Path)
		}
		if !slices.Contains(Groups, k.Group) {
			t.Errorf("%s: group %q is not in Groups", k.Path, k.Group)
		}
		s := k.Shape()
		walkShapes(k.Path, &s, func(p string, s *Shape) {
			if (s.Min != nil || s.Max != nil) && s.Type != TypeInt {
				t.Errorf("%s: min/max on a %s", p, s.Type)
			}
			if len(s.Enum) > 0 && s.Type != TypeString {
				t.Errorf("%s: enum on a %s", p, s.Type)
			}
			if s.Format != "" && s.Type != TypeString {
				t.Errorf("%s: format on a %s", p, s.Type)
			}
			if s.Items != nil && s.Type != TypeList && s.Type != TypeStringList {
				t.Errorf("%s: items on a %s", p, s.Type)
			}
			if (s.Keys != nil || s.Values != nil) && s.Type != TypeMap {
				t.Errorf("%s: keys/values on a %s", p, s.Type)
			}
			if len(s.Fields) > 0 && s.Type != TypeObject {
				t.Errorf("%s: fields on a %s", p, s.Type)
			}
			if (s.Type == TypeList || s.Type == TypeStringList) && s.Items == nil {
				t.Errorf("%s: a %s without items", p, s.Type)
			}
			if s.Type == TypeMap && s.Values == nil {
				t.Errorf("%s: a map without values", p)
			}
			for _, o := range s.Enum {
				if o.Value == "" || o.Label == "" {
					t.Errorf("%s: enum option %+v needs a value and a label", p, o)
				}
			}
			for _, f := range s.Fields {
				if f.Name == "" || f.Label == "" {
					t.Errorf("%s: field %+v needs a name and a label", p, f.Name)
				}
				if f.Default != nil {
					if err := f.Check(p+"."+f.Name, jsonForm(t, f.Default)); err != nil {
						t.Errorf("%s.%s: default %v is invalid: %v", p, f.Name, f.Default, err)
					}
				}
			}
		})
	}
}

// TestSchemaHelpComplete: every key, every enum option and every nested
// field (inside items, keys, values and fields alike) has a label and a
// help text, so the Settings page, `kudurru config schema` and the
// settings reference never show a bare control.
func TestSchemaHelpComplete(t *testing.T) {
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	for _, k := range Schema {
		if blank(k.Label) {
			t.Errorf("%s: no label", k.Path)
		}
		if blank(k.Help) {
			t.Errorf("%s: no help", k.Path)
		}
		s := k.Shape()
		walkShapes(k.Path, &s, func(p string, s *Shape) {
			for _, o := range s.Enum {
				if blank(o.Label) {
					t.Errorf("%s: enum option %q has no label", p, o.Value)
				}
				if blank(o.Help) {
					t.Errorf("%s: enum option %q has no help", p, o.Value)
				}
			}
			for _, f := range s.Fields {
				if blank(f.Label) {
					t.Errorf("%s.%s: no label", p, f.Name)
				}
				if blank(f.Help) {
					t.Errorf("%s.%s: no help", p, f.Name)
				}
			}
		})
	}
}

// TestSchemaDefaultsValidate: every key's default passes its own
// metadata, and the defaults together pass Validate.
func TestSchemaDefaultsValidate(t *testing.T) {
	for _, k := range Schema {
		s := k.Shape()
		if err := s.Check(k.Path, jsonForm(t, k.Default)); err != nil {
			t.Errorf("%s: default %v: %v", k.Path, k.Default, err)
		}
		for _, p := range k.Presets {
			if err := validateWith(t, k.Path, p); err != nil {
				t.Errorf("%s: preset %q: %v", k.Path, p, err)
			}
		}
	}
	if _, err := Load("", nil); err != nil {
		t.Fatalf("defaults: %v", err)
	}
}

// TestSchemaEnumsRejectBogus: every closed set rejects a value outside it,
// naming the key and the allowed values; top-level keys through Validate,
// nested ones through their shape.
func TestSchemaEnumsRejectBogus(t *testing.T) {
	const bogus = "bogus-value"
	enums := 0
	for _, k := range Schema {
		if len(k.Enum) > 0 {
			enums++
			err := validateWith(t, k.Path, bogus)
			if err == nil {
				t.Errorf("%s=%s accepted", k.Path, bogus)
				continue
			}
			fes := FieldErrors(err)
			if len(fes) != 1 || fes[0].Path != k.Path {
				t.Errorf("%s: field errors %v", k.Path, fes)
				continue
			}
			want := k.Shape().EnumValues()
			if !slices.Equal(fes[0].Allowed, want) || !strings.Contains(err.Error(), strings.Join(want, ", ")) {
				t.Errorf("%s: %v does not list %v", k.Path, err, want)
			}
			for _, v := range want {
				if k.Path == "vip.observation.provider" && v != "none" {
					continue
				} // provider-specific prerequisites are covered by observation tests
				if v == "customIP" || v == "blockPage" || k.Path == "audit.store" && (v == "auditlog" || v == "database") {
					continue // needs blocking.customIP.v4/v6, blockPage.enabled, store.driver kube, database.dsn
				}
				if err := validateWith(t, k.Path, v); err != nil {
					t.Errorf("%s=%s rejected: %v", k.Path, v, err)
				}
			}
		}
		s := k.Shape()
		walkShapes(k.Path, &s, func(p string, s *Shape) {
			if len(s.Enum) == 0 || p == k.Path {
				return
			}
			enums++
			err := s.Check(p, bogus)
			var fe *FieldError
			if !errors.As(err, &fe) || !slices.Equal(fe.Allowed, s.EnumValues()) {
				t.Errorf("%s: %v", p, err)
			}
			for _, v := range s.EnumValues() {
				if err := s.Check(p, v); err != nil {
					t.Errorf("%s=%s rejected: %v", p, v, err)
				}
			}
		})
	}
	if enums < 19 {
		t.Fatalf("only %d closed sets found", enums)
	}
}

func TestSchemaBounds(t *testing.T) {
	for _, k := range Schema {
		if k.Min != nil {
			if err := validateWith(t, k.Path, *k.Min-1); err == nil || !strings.Contains(err.Error(), k.Path) {
				t.Errorf("%s=%d: %v", k.Path, *k.Min-1, err)
			}
			if err := validateWith(t, k.Path, *k.Min); err != nil {
				t.Errorf("%s=%d: %v", k.Path, *k.Min, err)
			}
		}
		if k.Max != nil {
			if err := validateWith(t, k.Path, *k.Max+1); err == nil || !strings.Contains(err.Error(), k.Path) {
				t.Errorf("%s=%d: %v", k.Path, *k.Max+1, err)
			}
			if err := validateWith(t, k.Path, *k.Max); err != nil {
				t.Errorf("%s=%d: %v", k.Path, *k.Max, err)
			}
		}
	}
}

func TestSchemaSpecialEnums(t *testing.T) {
	for _, tc := range []struct {
		path string
		v    any
		ok   bool
	}{
		{"log.level", "info,dns=debug", true},
		{"log.level", "loud,dns=debug", false},
		{"auth.kubernetes.mode", "impersonate", false}, // Auth.validate explains D55
		{"upstreams.groups", map[string]any{"default": map[string]any{"servers": []string{"1.1.1.1"}}}, true},
		{"upstreams.groups", map[string]any{"default": map[string]any{"servers": []string{"cloudflare=tls://1.1.1.1", "https://dns.google/dns-query", "[2606:4700::1111]:53", "dns.quad9.net:53", "sdns://AgcAAAAAAAAABzEuMC4wLjE"}, "strategy": "rtt_weighted", "timeout": "1s"}}, true},
		{"upstreams.groups", map[string]any{"default": map[string]any{"servers": []string{"ftp://x"}}}, false},
		{"upstreams.groups", map[string]any{"default": map[string]any{"servers": []string{"1.1.1.1:99999"}}}, false},
		{"listeners.dns", []map[string]any{{"address": ":53"}}, true},
		{"listeners.dns", []map[string]any{{"address": "53"}}, false},
		{"listeners.dns", []map[string]any{{"address": ":53", "protocols": []string{"sctp"}}}, false},
		{"tsigKeys", []map[string]any{{"name": "k.", "algorithm": "HMAC-SHA256.", "secretFile": "/s"}}, true},
		{"tsigKeys", []map[string]any{{"name": "not a name", "algorithm": "hmac-sha256", "secretFile": "/s"}}, false},
		{"rbac.bindings", []map[string]any{{"role": "admin", "subjects": []map[string]any{{"kind": "User", "name": "a"}}}}, true},
		{"rbac.bindings", []map[string]any{{"role": "root", "subjects": []map[string]any{{"kind": "User", "name": "a"}}}}, false},
		{"rbac.bindings", []map[string]any{{"role": "admin", "scope": map[string]any{"kind": "planet"}, "subjects": []map[string]any{{"kind": "User", "name": "a"}}}}, false},
		{"rbac.bindings", []map[string]any{{"role": "admin", "subjects": []map[string]any{{"kind": "Robot", "name": "a"}}}}, false},
	} {
		err := validateWith(t, tc.path, tc.v)
		if (err == nil) != tc.ok {
			t.Errorf("%s=%v: err %v, want ok=%v", tc.path, tc.v, err, tc.ok)
		}
	}
}

func TestFormats(t *testing.T) {
	for _, tc := range []struct {
		f    Format
		good []string
		bad  []string
	}{
		{FormatIP, []string{"192.0.2.1", "2001:db8::1"}, []string{"fe80::1%eth0", "host", "10.0.0.0/8"}},
		{FormatIPv4, []string{"192.0.2.1"}, []string{"::1", "::ffff:10.0.0.1", "1.2.3"}},
		{FormatIPv6, []string{"2001:db8::1", "::"}, []string{"10.0.0.1", "::ffff:10.0.0.1"}},
		{FormatCIDR, []string{"10.0.0.0/8", "192.0.2.1", "2001:db8::/32"}, []string{"10.0.0.0/33", "example.com"}},
		{FormatHostPort, []string{":53", "0.0.0.0:53", "[::]:8080", "localhost:80"}, []string{"53", "0.0.0.0", "a:b", ":70000"}},
		{FormatURL, []string{"https://example.com/x", "http://10.0.0.1:8080"}, []string{"ftp://x", "https://", "example.com"}},
		{FormatDomain, []string{"example.com", "example.com.", "_dmarc.example", "localhost"}, []string{"-a.example", "a..b", "a b", strings.Repeat("a", 64) + ".com"}},
		{FormatUpstream, []string{"1.1.1.1", "1.1.1.1:53", "tls://one.one.one.one", "quic://dns.adguard-dns.com", "h3://dns.google/dns-query", "q=udp://9.9.9.9:53"}, []string{"ftp://x", "tls://", "1.1.1.1:", "a b"}},
		{FormatEmail, []string{"admin@example.com"}, []string{"admin", "Admin <admin@example.com>"}},
	} {
		for _, v := range tc.good {
			if msg := checkFormat(tc.f, v); msg != "" {
				t.Errorf("%s %q: %s", tc.f, v, msg)
			}
		}
		for _, v := range tc.bad {
			if checkFormat(tc.f, v) == "" {
				t.Errorf("%s %q accepted", tc.f, v)
			}
		}
	}
}

// TestRBACRolesMatchPermissions keeps the rbac.bindings role enum in step
// with internal/rbac/permissions.yaml.
func TestRBACRolesMatchPermissions(t *testing.T) {
	p, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range rbacRoles {
		got = append(got, o.Value)
	}
	want := p.RoleNames()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("rbac.bindings role enum %v, permissions.yaml %v", got, want)
	}
}

func TestFieldErrorsPaths(t *testing.T) {
	err := validateWith(t, "querylog.sink", "kafka")
	fes := FieldErrors(err)
	if len(fes) != 1 || fes[0].Path != "querylog.sink" || !slices.Equal(fes[0].Allowed, []string{"sqlite", "slog"}) {
		t.Fatalf("%v → %+v", err, fes)
	}
	if FieldErrors(errors.New("x")) != nil {
		t.Fatal("plain error yields field errors")
	}
}
