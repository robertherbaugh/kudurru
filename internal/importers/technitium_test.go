package importers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestTechnitiumCatalogQueryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, member, catalog string
		catalogStatus         int
		wantAccess            v1alpha1.AccessMode
		wantACL               []string
		wantCatalogCalls      int
		wantWarning           string
	}{
		{
			name:       "standalone",
			member:     `{"queryAccess":"Allow"}`,
			wantAccess: v1alpha1.AccessAllow,
		},
		{
			name:       "inherited ordered ACL replaces local allow",
			member:     `{"catalog":"cluster-catalog.test","overrideCatalogQueryAccess":false,"queryAccess":"Allow","queryAccessNetworkACL":[]}`,
			catalog:    `{"type":"Catalog","queryAccess":"UseSpecifiedNetworkACL","queryAccessNetworkACL":["!192.0.2.3","192.0.2.0/24"]}`,
			wantAccess: v1alpha1.AccessUseNetworkACL,
			wantACL:    []string{"!192.0.2.3", "192.0.2.0/24"}, wantCatalogCalls: 1,
		},
		{
			name:       "inherited deny replaces local allow",
			member:     `{"catalog":"cluster-catalog.test","overrideCatalogQueryAccess":false,"queryAccess":"Allow"}`,
			catalog:    `{"type":"Catalog","queryAccess":"Deny"}`,
			wantAccess: v1alpha1.AccessDeny, wantCatalogCalls: 1,
		},
		{
			name:       "inherited private networks",
			member:     `{"catalog":"cluster-catalog.test","overrideCatalogQueryAccess":false,"queryAccess":"Allow"}`,
			catalog:    `{"type":"Catalog","queryAccess":"AllowOnlyPrivateNetworks"}`,
			wantAccess: v1alpha1.AccessAllowOnlyPrivate, wantCatalogCalls: 1,
		},
		{
			name:          "explicit deny override needs no catalog permission",
			member:        `{"catalog":"cluster-catalog.test","overrideCatalogQueryAccess":true,"queryAccess":"Deny"}`,
			catalogStatus: http.StatusForbidden, wantAccess: v1alpha1.AccessDeny,
		},
		{
			name:       "explicit allow override",
			member:     `{"catalog":"cluster-catalog.test","overrideCatalogQueryAccess":true,"queryAccess":"Allow"}`,
			catalog:    `{"type":"Catalog","queryAccess":"Deny"}`,
			wantAccess: v1alpha1.AccessAllow,
		},
		{
			name:        "missing inheritance flag cannot fall back to local allow",
			member:      `{"catalog":"cluster-catalog.test","queryAccess":"Allow"}`,
			wantWarning: "catalog query-access inheritance is unknown",
		},
		{
			name:          "unreadable catalog cannot fall back to local allow",
			member:        `{"catalog":"cluster-catalog.test","overrideCatalogQueryAccess":false,"queryAccess":"Allow"}`,
			catalogStatus: http.StatusForbidden, wantCatalogCalls: 1,
			wantWarning: "could not read query access from catalog cluster-catalog.test",
		},
		{
			name:    "unknown catalog access cannot fall back to local allow",
			member:  `{"catalog":"cluster-catalog.test","overrideCatalogQueryAccess":false,"queryAccess":"Allow"}`,
			catalog: `{"type":"Catalog","queryAccess":"FutureMode"}`, wantCatalogCalls: 1,
			wantWarning: "query access policy needs manual conversion",
		},
		{
			name:        "unknown local access",
			member:      `{"queryAccess":"FutureMode"}`,
			wantWarning: "query access policy needs manual conversion",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalogCalls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/zones/list":
					_, _ = fmt.Fprint(w, `{"status":"ok","response":{"zones":[{"name":"one.test","type":"Primary"},{"name":"two.test","type":"Primary"},{"name":"cluster-catalog.test","type":"Catalog"},{"name":"signed.test","type":"Primary","dnssecStatus":"Signed"},{"name":"disabled.test","type":"Primary","disabled":true}]}}`)
				case "/api/settings/get":
					_, _ = fmt.Fprint(w, `{"status":"ok","response":{"enableBlocking":true,"blockingType":"AnyAddress"}}`)
				case "/api/allowed/export", "/api/blocked/export":
					w.WriteHeader(http.StatusOK)
				case "/api/zones/options/get":
					body := tc.member
					if r.URL.Query().Get("zone") == "cluster-catalog.test" {
						catalogCalls++
						if tc.catalogStatus != 0 {
							w.WriteHeader(tc.catalogStatus)
							return
						}
						body = tc.catalog
					}
					_, _ = fmt.Fprintf(w, `{"status":"ok","response":%s}`, body)
				case "/api/zones/records/get":
					_, _ = fmt.Fprint(w, `{"status":"ok","response":{"records":[{"type":"A"}]}}`)
				case "/api/zones/export":
					name := "one.test"
					switch r.URL.Query().Get("zone") {
					case "one.test":
					case "two.test":
						name = "two.test"
					default:
						t.Error("unexpected zone export")
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", "text/plain; charset=utf-8")
					_, _ = fmt.Fprintf(w, "$ORIGIN %s.\n@ 300 IN SOA ns.test. hostmaster.test. 1 3600 600 86400 300\n@ 300 IN NS ns.test.\nnas 120 IN A 192.0.2.10\n", name)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			p, err := Preview(context.Background(), MigrationRequest{Source: "technitium", URL: srv.URL, Credential: "test-token"})
			if err != nil {
				t.Fatal(err)
			}
			validatePlan(t, p)
			if catalogCalls != tc.wantCatalogCalls {
				t.Errorf("catalog fetched %d times, want %d (shared by both members)", catalogCalls, tc.wantCatalogCalls)
			}
			zones, records := 0, 0
			for _, obj := range p.Objects {
				if obj["kind"] == "DNSRecordSet" {
					records++
				}
				if obj["kind"] != "DNSZone" {
					continue
				}
				zones++
				var dz v1alpha1.DNSZone
				b, _ := json.Marshal(obj)
				if err := json.Unmarshal(b, &dz); err != nil {
					t.Fatal(err)
				}
				want := v1alpha1.ZonePolicy{QueryAccess: tc.wantAccess, QueryACL: tc.wantACL, ZoneTransfer: "deny", Notify: "none"}
				if !reflect.DeepEqual(dz.Spec.Policy, want) {
					t.Errorf("policy=%+v, want %+v", dz.Spec.Policy, want)
				}
			}
			if tc.wantAccess == "" {
				if zones != 0 || records != 0 {
					t.Fatalf("unresolved policy imported %d zones and %d record sets", zones, records)
				}
			} else if zones != 2 || records != 2 {
				t.Fatalf("imported %d zones and %d record sets, want both primary zones with records", zones, records)
			}
			warnings := strings.Join(p.Warnings, "\n")
			if tc.wantWarning != "" && !strings.Contains(warnings, tc.wantWarning) {
				t.Errorf("missing warning %q in %s", tc.wantWarning, warnings)
			}
			if tc.wantAccess != "" && strings.Contains(tc.member, `"catalog"`) && strings.Count(warnings, "Catalog member zones are imported as independent primary zones") != 1 {
				t.Error("expected one catalog conversion warning shared by both imported members")
			}
			for _, skipped := range []string{"Skipped Catalog zone", "Skipped signed zone", "Skipped disabled zone"} {
				if !strings.Contains(warnings, skipped) {
					t.Errorf("missing warning %q", skipped)
				}
			}
		})
	}
}
