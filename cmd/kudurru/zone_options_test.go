package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCLIZoneOptionsRealRouter(t *testing.T) {
	f := newRouterCLI(t)
	for _, name := range []string{"home", "owned"} {
		manager := store.ManagerAPI
		if name == "owned" {
			manager = "file:zones.yaml"
		}
		zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"keep": "label"}, Annotations: map[string]string{"keep": "annotation"}}, Spec: public.DNSZoneSpec{ZoneName: name + ".example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + name + ".example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}}
		if _, err := f.store.Apply(t.Context(), zone, store.ApplyOptions{Manager: manager}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) zoneOptionsDocument {
		t.Helper()
		out, _, err := f.run("zones", "options", "get", name, "-o", "json")
		var doc zoneOptionsDocument
		if err != nil || json.Unmarshal([]byte(out), &doc) != nil || doc.ResourceVersion == "" {
			t.Fatal("zone option review failed", err)
		}
		return doc
	}
	write := func(name string, doc zoneOptionsDocument) error {
		t.Helper()
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		f.stdin = string(encoded)
		_, _, err = f.run("zones", "options", "set", name, "-f", "-", "-o", "json")
		return err
	}
	reviewed := read("home")
	var original public.DNSZone
	key := store.Key{Kind: "DNSZone", Name: "home"}
	if err := f.store.Get(t.Context(), key, &original); err != nil {
		t.Fatal(err)
	}
	var spec public.DNSZoneSpec
	if err := json.Unmarshal(reviewed.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	spec.DefaultTTL = 120
	reviewed.Spec, _ = json.Marshal(spec)
	if err := write("home", reviewed); err != nil {
		t.Fatal("zone options save failed", err)
	}
	var saved public.DNSZone
	if err := f.store.Get(t.Context(), key, &saved); err != nil || saved.Spec.DefaultTTL != 120 || saved.UID != original.UID || saved.Labels["keep"] != "label" || saved.Annotations["keep"] != "annotation" {
		t.Fatal("zone options replaced metadata or lost the selected value", err)
	}
	current := read("home")
	if err := write("home", current); err != nil {
		t.Fatal("authorized no-op failed", err)
	}
	if f.count(http.MethodPut, "/api/v1/objects/dnszones/home") != 2 {
		t.Fatal("no-op skipped its authorized PUT or a write was replayed")
	}
	if err := write("home", reviewed); !apiclient.IsStatus(err, http.StatusPreconditionFailed) {
		t.Fatal("stale review was not refused", err)
	}
	if f.count(http.MethodPut, "/api/v1/objects/dnszones/home") != 3 {
		t.Fatal("stale review was replayed")
	}
	invalid := current
	invalid.Spec = json.RawMessage(`{"zoneName":"bad..example.","type":"primary"}`)
	if err := write("home", invalid); !apiclient.IsKey(err, api.KeyInvalid) {
		t.Fatal("invalid zone options were not refused", err)
	}
	if err := write("owned", read("owned")); !apiclient.IsKey(err, api.KeyManagedBy) {
		t.Fatal("unchanged externally managed zone was not refused", err)
	}
	f.env["KUDURRU_TOKEN"] = apitest.BearerPrefix + "viewer"
	if err := write("home", current); !apiclient.IsKey(err, api.KeyForbidden) {
		t.Fatal("viewer no-op was not authorized by the server", err)
	}
	if err := f.store.Get(t.Context(), key, &saved); err != nil || saved.ResourceVersion != current.ResourceVersion {
		t.Fatal("a refused options write changed the zone")
	}
}

func TestCLIZoneOptionsRejectsInvalidDocuments(t *testing.T) {
	f := newRouterCLI(t)
	for _, body := range []string{
		`{}`, `{"spec":{}}`, `{"resourceVersion":"1","spec":null}`,
		`{"resourceVersion":"1","spec":{"unrecognized":true}}`,
		`{"resourceVersion":"1","spec":{},"metadata":{}}`,
		`{"resourceVersion":"1","resourceVersion":"2","spec":{}}`,
		`{"resourceVersion":"1","spec":{"defaultTTL":60,"defaultTTL":120}}`,
		"resourceVersion: '1'\nresourceVersion: '2'\nspec: {}\n",
		"resourceVersion: '1'\nspec: {}\n---\nresourceVersion: '2'\nspec: {}\n",
		strings.Repeat(" ", zoneOptionsMaxBytes+1),
	} {
		f.stdin = body
		if _, _, err := f.run("zones", "options", "set", "home", "-f", "-"); err == nil {
			t.Fatal("invalid options document accepted")
		}
	}
	if f.count(http.MethodGet, "/api/v1/objects/dnszones/home") != 0 || f.count(http.MethodPut, "/api/v1/objects/dnszones/home") != 0 {
		t.Fatal("invalid input reached the server")
	}
}

func TestCLIZoneOptionsCurrentSQLiteFinalCredential(t *testing.T) {
	for _, mode := range []string{"save", "expired-update", "expired-noop"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "options"}, Spec: public.DNSZoneSpec{ZoneName: "options.test.", Type: public.ZoneTypeForward}}
			if _, err := f.s.opts.Store.Apply(t.Context(), zone, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
			awaitServingZoneCommandForward(t, f, zone.Name)
			var reachedFinal atomic.Bool
			command := func(ctx context.Context, key store.Key, fn func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error) error {
				return f.s.ObjectCommand(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
					if err := fn(ctx, tx, authorize); err != nil {
						return err
					}
					reachedFinal.Store(true)
					if mode != "save" {
						// Expire the original credential after the actual route has
						// selected/staged its result, before transaction commit.
						f.offset.Store(int64(f.expires.Sub(f.now)))
					}
					return nil
				})
			}
			srv := servingObjectAPI(t, f, command)
			t.Cleanup(srv.Close)
			var puts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					puts.Add(1)
				}
				srv.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			h := &harness{t: t, cfgPath: filepath.Join(t.TempDir(), "cli.yaml"), env: map[string]string{"KUDURRU_TOKEN": f.token}, keyring: &memKeyring{}}
			out, _, err := h.run("--server", server.URL, "zones", "options", "get", zone.Name, "-o", "json")
			var doc zoneOptionsDocument
			if err != nil || json.Unmarshal([]byte(out), &doc) != nil || doc.ResourceVersion == "" {
				t.Fatal("actual SQLite options review failed", err)
			}
			if mode != "expired-noop" {
				var spec public.DNSZoneSpec
				if err := json.Unmarshal(doc.Spec, &spec); err != nil {
					t.Fatal(err)
				}
				spec.DefaultTTL = 120
				doc.Spec, _ = json.Marshal(spec)
			}
			body, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			h.stdin = string(body)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = h.run("--server", server.URL, "zones", "options", "set", zone.Name, "-f", "-", "-o", "json")
			if !reachedFinal.Load() || puts.Load() != 1 {
				t.Fatal("options command skipped final admission or replayed its write", err)
			}
			if mode == "save" {
				var saved public.DNSZone
				if err != nil || f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: zone.Name}, &saved) != nil || saved.Spec.DefaultTTL != 120 {
					t.Fatal("actual SQLite options save failed", err)
				}
				return
			}
			if !apiclient.IsStatus(err, http.StatusUnauthorized) {
				t.Fatal("original credential expiry was not refused", err)
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("expired options command changed durable state", err)
			}
		})
	}
}
