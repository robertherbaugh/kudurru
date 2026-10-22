package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
)

func TestRunServeSQLiteCompoundZoneCommands(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	for restart := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("zone mount shutdown failed", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("zone mount shutdown did not join")
			}
		}
		t.Cleanup(stop)
		until := time.Now().Add(15 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("zone mount startup ended", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(until) {
				t.Fatal("zone mount never became ready")
			}
		}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + api.BasePath,
			http: &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: mountedRetryTransport{}}}
		var login api.SessionInfo
		client.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		client.csrf = login.CSRF
		if restart == 0 {
			// This request creates both the zone and its record set through one
			// current command in the actual mounted server.
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.base+"/zones/compound/import?origin=compound.test.", strings.NewReader(servingZoneFile(1)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "text/dns")
			req.Header.Set("X-Kudurru-CSRF", client.csrf)
			res, err := client.http.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var imported api.ImportResult
			err = json.NewDecoder(res.Body).Decode(&imported)
			_ = res.Body.Close()
			if err != nil || res.StatusCode != http.StatusOK || !imported.Created || len(imported.RecordSets) != 1 {
				t.Fatalf("mounted import failed: status=%d decode=%v", res.StatusCode, err)
			}
			awaitMountedDNS(t, cfg, "h0.compound.test.", "192.0.2.1")
			endpoint := public.Endpoint{DNSName: "added", RecordType: "A", Targets: []string{"192.0.2.80"}, RecordTTL: 60}
			client.call(http.MethodPost, "/zones/compound/records", endpoint, http.StatusCreated, nil)
			awaitMountedDNS(t, cfg, "added.compound.test.", "192.0.2.80")
			endpoint.DNSName = "removed"
			client.call(http.MethodPost, "/zones/compound/records", endpoint, http.StatusCreated, nil)
			client.call(http.MethodDelete, "/zones/compound/records/removed/A", nil, http.StatusNoContent, nil)
			var dry api.ZoneDeleteResult
			client.call(http.MethodDelete, "/zones/compound?dryRun=true", nil, http.StatusOK, &dry)
			if !dry.DryRun || dry.RecordSetCount != 2 {
				t.Fatal("mounted cascade dry run lost the complete current roster")
			}
		}
		// Both imported and edited authority survive the actual server restart.
		awaitMountedDNS(t, cfg, "h0.compound.test.", "192.0.2.1")
		awaitMountedDNS(t, cfg, "added.compound.test.", "192.0.2.80")
		// RR-only desired edits deliberately retain the last committed head.
		// Observe the deletion's actual DNS publication before inspecting the
		// served-record diagnostic, both before and after server restart.
		awaitMountedDNSAbsent(t, cfg, "removed.compound.test.")
		var records api.List[api.ZoneRecord]
		client.call(http.MethodGet, "/zones/compound/records", nil, http.StatusOK, &records)
		for _, record := range records.Items {
			if record.DNSName == "removed.compound.test" {
				t.Fatal("deleted endpoint survived publication or restart")
			}
		}
		if restart == 1 {
			var deleted api.ZoneDeleteResult
			client.call(http.MethodDelete, "/zones/compound", nil, http.StatusOK, &deleted)
			if deleted.DryRun || deleted.RecordSetCount != 2 {
				t.Fatal("mounted cascade did not delete both record sets")
			}
			for _, key := range []string{"dnszones/compound", "dnsrecordsets/default/api-compound", "dnsrecordsets/default/import-compound-1"} {
				client.call(http.MethodGet, "/objects/"+key, nil, http.StatusNotFound, nil)
			}
			client.call(http.MethodPost, "/zones/compound/notify", nil, http.StatusNotFound, nil)
		} else {
			client.call(http.MethodPost, "/zones/compound/notify", nil, http.StatusAccepted, nil)
		}
		stop()
	}
}
