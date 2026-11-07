package providerresources

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

func readyPublicCLI(t *testing.T, f *browserFixture, origin servingauth.ProviderOrigin) (*Owner, Desired, *Lease, *CLI) {
	t.Helper()
	f.mu.Lock()
	var m map[string]any
	_ = json.Unmarshal(f.discovery, &m)
	m["token_endpoint_auth_methods_supported"] = []string{"client_secret_basic", "none"}
	m["response_types_supported"] = []string{"code"}
	f.discovery, _ = json.Marshal(m)
	f.mu.Unlock()
	s := f.source(t, origin)
	s.Spec.CLIClientID = "cli"
	p, e := oidccore.Compile(s.Spec, s.Ref.ID)
	if e != nil {
		t.Fatal("compile public source")
	}
	s.Spec, s.PolicyIdentity = p.Spec(), p.Identity()
	s.SpecIdentity = hash(struct {
		Profile   string
		Ref       servingauth.ProviderRef
		CreatedAt time.Time
		Spec      any
	}{"servingauth/provider/v1", s.Ref, s.CreatedAt, s.Spec})
	d := Desired{RegistryDigest: hash("public-cli-registry"), Host: f.host(t, s), Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	d = admit(t, o, d, candidate(t, o))
	l, e := o.Acquire(t.Context(), d)
	if e != nil {
		t.Fatal("capture public source")
	}
	t.Cleanup(l.Release)
	id, _ := servingauth.ProviderResourceIdentity(s, l.Resources()[0])
	c, e := l.CLI(s.Ref, id)
	if e != nil {
		t.Fatal("public selection")
	}
	return o, d, l, c
}
func TestCLIPublicCapabilityOriginalCredentialSourceAndTime(t *testing.T) {
	for _, origin := range []servingauth.ProviderOrigin{servingauth.ProviderConfig, servingauth.ProviderObject} {
		t.Run(string(origin), func(t *testing.T) {
			for _, mode := range []string{"current", "credential", "host", "resource", "released", "expired", "cancel"} {
				t.Run(mode, func(t *testing.T) {
					f := newBrowserFixture(t)
					o, d, l, c := readyPublicCLI(t, f, origin)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					switch mode {
					case "credential":
						f.secret.Store("rotated-test-only")
					case "host":
						var current atomic.Bool
						current.Store(true)
						c, _ = c.BindCurrent(current.Load)
						current.Store(false)
					case "resource":
						d.RegistryDigest = hash("replaced-registry")
						if o.SetDesired(t.Context(), d) != nil {
							t.Fatal("replace fixture")
						}
					case "released":
						l.Release()
					case "expired":
						f.clock.Add(int64(time.Minute))
					case "cancel":
						cancel()
					}
					if (c.Check(ctx) == nil) != (mode == "current") {
						t.Fatal("original CLI gate mismatch")
					}
					if f.requests.Load() != 0 {
						t.Fatal("CLI capability contacted OP token endpoint")
					}
				})
			}
		})
	}
}
func TestCLIDiscoveryAdvertisedPublicSupportAndBinding(t *testing.T) {
	f := newBrowserFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	h := f.host(t, s)
	var base map[string]any
	_ = json.Unmarshal(f.discovery, &base)
	base["token_endpoint_auth_methods_supported"] = []string{"client_secret_basic", "none"}
	base["response_types_supported"] = []string{"code"}
	raw, _ := json.Marshal(base)
	m, e := parseMetadata(raw, s.Issuer, h.network)
	if e != nil || !m.cliBrowser || !m.browser {
		t.Fatal("advertised public and web support")
	}
	bound := metadataBinding(m)
	for _, field := range []string{"token_endpoint_auth_methods_supported", "code_challenge_methods_supported", "response_types_supported"} {
		copyFields := map[string]any{}
		for k, v := range base {
			copyFields[k] = v
		}
		delete(copyFields, field)
		b, _ := json.Marshal(copyFields)
		got, e := parseMetadata(b, s.Issuer, h.network)
		if e != nil || got.cliBrowser || metadataBinding(got) == bound {
			t.Fatal("missing advertisement established public support")
		}
	}
	base["grant_types_supported"] = []string{"client_credentials"}
	raw, _ = json.Marshal(base)
	m, e = parseMetadata(raw, s.Issuer, h.network)
	if e != nil || m.cliBrowser {
		t.Fatal("incompatible grant accepted")
	}
}

func TestCLIDeviceDiscoveryAdvertisementAndAuthorityBinding(t *testing.T) {
	f := newBrowserFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	h := f.host(t, s)
	var base map[string]any
	_ = json.Unmarshal(f.discovery, &base)
	base["token_endpoint_auth_methods_supported"] = []string{"client_secret_basic", "none"}
	base["device_authorization_endpoint"] = s.Issuer + "/device"
	base["grant_types_supported"] = []string{"urn:ietf:params:oauth:grant-type:device_code"}
	raw, _ := json.Marshal(base)
	m, e := parseMetadata(raw, s.Issuer, h.network)
	if e != nil || !m.cliDevice || m.cliBrowser {
		t.Fatal("device support coupled to browser or not advertised")
	}
	bound := metadataBinding(m)
	for _, field := range []string{"token_endpoint_auth_methods_supported", "device_authorization_endpoint", "grant_types_supported"} {
		fields := map[string]any{}
		for k, v := range base {
			fields[k] = v
		}
		delete(fields, field)
		b, _ := json.Marshal(fields)
		got, e := parseMetadata(b, s.Issuer, h.network)
		if e != nil || got.cliDevice || metadataBinding(got) == bound {
			t.Fatal("missing device capability retained authority")
		}
	}
	base["device_authorization_endpoint"] = s.Issuer + "/replacement"
	raw, _ = json.Marshal(base)
	changed, e := parseMetadata(raw, s.Issuer, h.network)
	if e != nil || !changed.cliDevice || metadataBinding(changed) == bound {
		t.Fatal("device endpoint unbound")
	}
	base["device_authorization_endpoint"] = "http://untrusted.example/device"
	raw, _ = json.Marshal(base)
	if _, e := parseMetadata(raw, s.Issuer, h.network); e == nil {
		t.Fatal("untrusted device endpoint admitted")
	}
}
