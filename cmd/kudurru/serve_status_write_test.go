package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// A primary zone keeps answering from its committed data across the commits
// that follow every change of that data: the head commit (serial advance),
// the committed-status writer's status write (which advances the DNSZone
// resourceVersion) and the driver's realignment of the lifecycle receipt to
// that resourceVersion. Before CORE-STATUS-WRITE-SERVFAIL the publication
// between the status write and the realignment answered the zone with
// SERVFAIL (servfail_unsynced) for up to about half a second.
func TestRunServeSQLitePrimaryAnswersAcrossStatusWrites(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, secret := mountedTestConfig(t)
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []store.Object{
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}},
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{
			ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."},
			Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.example.", Types: []string{"A"}}}},
		}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "seed.example.", RecordType: "A", Targets: []string{"192.0.2.42"}}}}},
	} {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("serve ended", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("serve failed to join shutdown")
		}
	})
	awaitMountedDNS(t, cfg, "seed.example.", "192.0.2.42")

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + "/api/v1", http: &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: mountedRetryTransport{}}}
	var login api.SessionInfo
	client.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, 200, &login)
	client.csrf = login.CSRF
	zoneStatus := func() (string, uint32) {
		t.Helper()
		var zone map[string]any
		client.call(http.MethodGet, "/objects/dnszones/home", nil, 200, &zone)
		metadata, _ := zone["metadata"].(map[string]any)
		status, _ := zone["status"].(map[string]any)
		rv, _ := metadata["resourceVersion"].(string)
		serial, _ := status["serial"].(float64)
		return rv, uint32(serial)
	}
	// Let the first publication's own status write and realignment settle
	// before counting, so every counted status write follows an UPDATE.
	_, serial := zoneStatus()
	for deadline := time.Now().Add(30 * time.Second); serial == 0; _, serial = zoneStatus() {
		if time.Now().After(deadline) {
			t.Fatal("committed status never projected a serial")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The probe asks for a committed name without pause for the whole run.
	type outcome struct {
		answered, servfail, wrong, silent int
		first                             []string
	}
	var (
		mu    sync.Mutex
		seen  outcome
		stop  = make(chan struct{})
		probe sync.WaitGroup
	)
	probe.Add(1)
	go func() {
		defer probe.Done()
		resolver := &dns.Client{Timeout: 200 * time.Millisecond}
		for {
			select {
			case <-stop:
				return
			default:
			}
			query := new(dns.Msg)
			query.SetQuestion("seed.example.", dns.TypeA)
			answer, _, err := resolver.Exchange(query, cfg.Listeners.DNS[0].Address)
			mu.Lock()
			switch {
			case err != nil || answer == nil:
				seen.silent++
			case answer.Rcode == dns.RcodeServerFailure:
				seen.servfail++
				if len(seen.first) < 5 {
					seen.first = append(seen.first, fmt.Sprintf("%s at %s", answer.String(), time.Now().Format(time.RFC3339Nano)))
				}
			case answer.Rcode != dns.RcodeSuccess || !answer.Authoritative || len(answer.Answer) != 1:
				seen.wrong++
				if len(seen.first) < 5 {
					seen.first = append(seen.first, answer.String())
				}
			default:
				if a, ok := answer.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.42" {
					seen.wrong++
				} else {
					seen.answered++
				}
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()

	// Most changes are record-set edits through the API (the common case);
	// every fourth is a signed dynamic UPDATE. Each one commits a new head
	// and is followed by the committed-status writer's write of its serial.
	const changes = 24
	statusWrites, serials, updateRetries := 0, map[uint32]bool{serial: true}, 0
	rv, _ := zoneStatus()
	// An UPDATE reply follows the publication of its commit, which takes
	// more than two seconds under the race detector on two CPUs while the
	// probe runs. A shorter client timeout gives up on a request that then
	// commits, and every retry is a new request with its own commit.
	updater := &dns.Client{Net: "udp", Timeout: 15 * time.Second, TsigSecret: map[string]string{"updates.": secret}}
	for i := range changes {
		started := time.Now()
		name, address := "dynamic-"+strconv.Itoa(i)+".example.", "192.0.2.33"
		if i%4 == 0 {
			// An idempotent RR addition in a NEW request each time. A
			// request whose outcome the node cannot state gets no answer
			// or SERVFAIL; the client retries, as the restart test does.
			for deadline := time.Now().Add(60 * time.Second); ; {
				answer, _, err := updater.Exchange(primaryUpdateMessage("example.", name, true), cfg.Listeners.DNS[0].Address)
				if err == nil && (answer == nil || answer.IsTsig() == nil) {
					t.Fatalf("UPDATE %d failed verification", i)
				}
				if err == nil && answer.Rcode == dns.RcodeSuccess {
					break
				}
				if (err == nil && answer.Rcode != dns.RcodeServerFailure) || !time.Now().Before(deadline) {
					t.Fatalf("UPDATE %d failed after %s: %v %v", i, time.Since(started), err, answer)
				}
				updateRetries++
				time.Sleep(25 * time.Millisecond)
			}
		} else {
			name, address = "churn.example.", "198.51.100."+strconv.Itoa(i)
			code := http.StatusOK
			if i == 1 {
				code = http.StatusCreated
			}
			body := map[string]any{"apiVersion": public.GroupVersion.String(), "kind": "DNSRecordSet", "metadata": map[string]any{"name": "churn", "namespace": "default"}, "spec": map[string]any{"zoneRef": map[string]any{"name": "home"}, "endpoints": []any{map[string]any{"dnsName": name, "recordType": "A", "targets": []string{address}}}}}
			client.call(http.MethodPut, "/objects/dnsrecordsets/default/churn", body, code, nil)
		}
		awaitMountedDNS(t, cfg, name, address)
		// Wait for the committed-status writer's write of the new serial.
		for deadline := time.Now().Add(30 * time.Second); ; {
			nextRV, nextSerial := zoneStatus()
			if nextRV != rv && !serials[nextSerial] {
				rv = nextRV
				serials[nextSerial] = true
				statusWrites++
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("change %d: no status write of a new serial (rv %s, serial %d)", i, nextRV, nextSerial)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Cover the realignment after the last status write.
	time.Sleep(1500 * time.Millisecond)
	close(stop)
	probe.Wait()

	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d status writes of %d new serials; probe: %d answered, %d SERVFAIL, %d other, %d without an answer within 200 ms; %d UPDATE retries after SERVFAIL or no answer", statusWrites, len(serials)-1, seen.answered, seen.servfail, seen.wrong, seen.silent, updateRetries)
	if statusWrites < 20 || len(serials)-1 < 20 {
		t.Fatalf("only %d status writes and %d serial advances observed", statusWrites, len(serials)-1)
	}
	if seen.servfail != 0 || seen.wrong != 0 {
		t.Fatalf("committed primary zone did not answer from its committed data across status writes: %d SERVFAIL, %d other answers; first: %q", seen.servfail, seen.wrong, seen.first)
	}
	if seen.answered < 100 {
		t.Fatalf("probe answered only %d queries", seen.answered)
	}
}
