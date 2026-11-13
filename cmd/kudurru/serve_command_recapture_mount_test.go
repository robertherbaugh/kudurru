package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// recaptureMountedSend sends one request once and returns its status and
// body. It never retries.
func recaptureMountedSend(client *http.Client, base, method, path, csrf string, body any) (int, []byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-Kudurru-CSRF", csrf)
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, raw, err
}

// mountedRefusal classifies one answer of the load below.
type mountedRefusal int

const (
	mountedAdmitted mountedRefusal = iota
	// mountedSuperseded is the refusal D399 removes: a command captured on a
	// publication that another commit superseded before dispatch.
	mountedSuperseded
	// mountedBoundEnded is the same answer after the command waited its
	// whole five-second recapture bound (D399) because the node kept
	// publishing: the publication-starvation limit, not a superseded
	// command that D399 failed to capture again.
	mountedBoundEnded
	// mountedAtCapture is a request refused before it was read, because the
	// node had not published its newest commit within the capture wait (503
	// generation_pending). Nothing of it ran; D399 does not change it.
	mountedAtCapture
	// mountedUnacknowledged is a sign-in whose session committed but whose
	// publication did not arrive within the seal wait ("authentication
	// operation unavailable"). D399 does not change it either.
	mountedUnacknowledged
	mountedOther
)

func classifyMounted(status int, raw []byte, err error, ok ...int) mountedRefusal {
	switch {
	case err == nil && slices.Contains(ok, status):
		return mountedAdmitted
	case status == http.StatusServiceUnavailable && bytes.Contains(raw, []byte("current writable serving generation unavailable")):
		var answer struct {
			Took float64 `json:"took_ms"`
		}
		if json.Unmarshal(raw, &answer) == nil && answer.Took >= float64(commandRecaptureWait/time.Millisecond) {
			return mountedBoundEnded
		}
		return mountedSuperseded
	case status == http.StatusServiceUnavailable && bytes.Contains(raw, []byte(`"generation_pending"`)):
		return mountedAtCapture
	case status == http.StatusServiceUnavailable && bytes.Contains(raw, []byte("authentication operation unavailable")):
		return mountedUnacknowledged
	}
	return mountedOther
}

// A ready node that commits a steady stream of signed UPDATEs (10 per second,
// each followed by the node's own zone status work) never refuses a sign-in
// or a record-set write that it captured because one of those commits
// published while the command ran (D399): no answer is "current writable
// serving generation unavailable" before its five-second recapture bound
// ended. Every request is sent once.
//
// Before each command the test reads until the node serves a current
// publication, so the command is captured on one and the background commits
// land while it runs, which is the case D399 decides. The test does not
// require every request to succeed: on a loaded machine the node's
// publication falls behind a stream of commits (each commit cancels the
// preparation in progress), so a request can be refused at capture
// (generation_pending) or, for a sign-in, after its session committed
// ("authentication operation unavailable"), or, when the node keeps
// publishing for the whole bound, after five seconds with "current writable
// serving generation unavailable". Those are counted and logged;
// they belong to the open publication-starvation limit, not to D399. Any
// other failure fails the test, and at least one sign-in and one write must
// be admitted. The writer's own first sign-in is set-up, not a subject: it
// is sent again only after a refusal before it was read or after the whole
// bound, at most five times.
func TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates(t *testing.T) {
	cfg, secret := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.Cache.MaxEntries = 0
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "load"}, Spec: public.DNSZoneSpec{ZoneName: "load.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.load.test."},
			Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.load.test.", Types: []string{"A"}}}}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "load"}, Endpoints: []public.Endpoint{{DNSName: "seed.load.test.", RecordType: "A", Targets: []string{"192.0.2.1"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	defer stop()
	// The first answer comes before the zone's own first status write: the
	// load below starts right away, inside that cascade too.
	awaitMountedDNS(t, cfg, "seed.load.test.", "192.0.2.1")
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	login := api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}

	done := make(chan struct{})
	var updates sync.WaitGroup
	var mu sync.Mutex
	rcodes := map[int]int{}
	var sent atomic.Int64
	updates.Add(1)
	go func() {
		defer updates.Done()
		client := &dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigSecret: map[string]string{"updates.": secret}}
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			i := sent.Add(1)
			m := new(dns.Msg)
			m.SetUpdate("load.test.")
			// Replace one of 20 names' address: the zone's size stays constant.
			owner := fmt.Sprintf("u%d.load.test.", i%20)
			m.RemoveRRset([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeA, Class: dns.ClassINET}}})
			m.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP(fmt.Sprintf("192.0.2.%d", i%250+1))}})
			m.SetTsig("updates.", dns.HmacSHA256, 300, time.Now().Unix())
			rcode := -1
			if answer, _, err := client.Exchange(m, cfg.Listeners.DNS[0].Address); err == nil && answer != nil {
				rcode = answer.Rcode
			}
			mu.Lock()
			rcodes[rcode]++
			mu.Unlock()
		}
	}()
	time.Sleep(time.Second)

	const count = 30
	counts := map[mountedRefusal]int{}
	var failures []string
	signIns, writes := 0, 0
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := &http.Client{Jar: jar, Transport: &http.Transport{}, Timeout: 20 * time.Second}
	defer writer.CloseIdleConnections()
	// published waits, with reads only, until the node serves a current
	// publication, so that the next command is captured on one; the
	// background commits then land while that command runs.
	published := func() {
		for until := time.Now().Add(time.Minute); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
			if response, err := writer.Get(base + "/auth/status"); err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return
				}
			}
		}
		t.Fatal("the node did not publish for a minute")
	}
	var info api.SessionInfo
	for attempt := 0; info.CSRF == "" && attempt < 5; attempt++ {
		published()
		status, raw, err := recaptureMountedSend(writer, base, http.MethodPost, "/auth/login", "", login)
		switch class := classifyMounted(status, raw, err, http.StatusOK); class {
		case mountedAdmitted:
			if json.Unmarshal(raw, &info) != nil || info.CSRF == "" {
				t.Fatalf("writer sign-in: %s", raw)
			}
		case mountedAtCapture, mountedBoundEnded:
			counts[class]++
		default:
			t.Fatalf("writer sign-in: status %d err %v: %s", status, err, raw)
		}
	}
	if info.CSRF == "" {
		t.Fatal("the writer's sign-in was refused five times")
	}
	record := func(what string, i, status int, raw []byte, err error, ok ...int) bool {
		class := classifyMounted(status, raw, err, ok...)
		counts[class]++
		if class == mountedSuperseded || class == mountedOther {
			failures = append(failures, fmt.Sprintf("%s %d: status %d err %v: %s", what, i, status, err, raw))
		}
		return class == mountedAdmitted
	}
	for i := range count {
		client := &http.Client{Transport: &http.Transport{}, Timeout: 20 * time.Second}
		published()
		status, raw, err := recaptureMountedSend(client, base, http.MethodPost, "/auth/login", "", login)
		client.CloseIdleConnections()
		if record("sign-in", i, status, raw, err, http.StatusOK) {
			signIns++
		}
		// Five record sets replaced in turn keep the zone's size constant, so
		// the node's time to publish does not grow with the run.
		name := fmt.Sprintf("w%d", i%5)
		body := public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: "kudurru.io/v1alpha1", Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "load"}, Endpoints: []public.Endpoint{{DNSName: name + ".load.test.", RecordType: "A", Targets: []string{fmt.Sprintf("192.0.2.%d", i%250+1)}}}}}
		published()
		status, raw, err = recaptureMountedSend(writer, base, http.MethodPut, "/objects/dnsrecordsets/default/"+name, info.CSRF, body)
		if record("record-set write", i, status, raw, err, http.StatusOK, http.StatusCreated) {
			writes++
		}
	}
	close(done)
	updates.Wait()
	mu.Lock()
	t.Logf("signed UPDATEs sent %d, rcodes %v; of %d sign-ins and %d writes: admitted %d and %d; refused as superseded %d, after the 5 s bound %d, at capture %d, sign-in unacknowledged %d, other %d",
		sent.Load(), rcodes, count, count, signIns, writes, counts[mountedSuperseded], counts[mountedBoundEnded], counts[mountedAtCapture], counts[mountedUnacknowledged], counts[mountedOther])
	mu.Unlock()
	for _, failure := range failures {
		t.Error(failure)
	}
	if signIns == 0 || writes == 0 {
		t.Fatal("no sign-in or no record-set write was admitted during the load")
	}
	if rcodes[dns.RcodeSuccess] == 0 {
		t.Fatal("no signed UPDATE committed during the load")
	}
}
