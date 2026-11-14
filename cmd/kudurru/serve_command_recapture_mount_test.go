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

// mountedRefusal names one answer of the load below.
type mountedRefusal int

const (
	mountedAdmitted mountedRefusal = iota
	// mountedSuperseded is the refusal of a command whose publication
	// another commit superseded before dispatch, also after its five-second
	// recapture bound (D399).
	mountedSuperseded
	// mountedAtCapture is a request refused before it was read, because the
	// node had not published its newest commit within the capture wait (503
	// generation_pending).
	mountedAtCapture
	// mountedUnacknowledged is a sign-in whose session committed but whose
	// publication did not arrive within the seal wait ("authentication
	// operation unavailable").
	mountedUnacknowledged
	mountedOther
)

func (r mountedRefusal) String() string {
	return [...]string{"admitted", "refused as superseded", "refused at capture", "sign-in unacknowledged", "other"}[r]
}

func classifyMounted(status int, raw []byte, err error, ok ...int) mountedRefusal {
	switch {
	case err == nil && slices.Contains(ok, status):
		return mountedAdmitted
	case status == http.StatusServiceUnavailable && bytes.Contains(raw, []byte("current writable serving generation unavailable")):
		return mountedSuperseded
	case status == http.StatusServiceUnavailable && bytes.Contains(raw, []byte(`"generation_pending"`)):
		return mountedAtCapture
	case status == http.StatusServiceUnavailable && bytes.Contains(raw, []byte("authentication operation unavailable")):
		return mountedUnacknowledged
	}
	return mountedOther
}

// mountedPublicationLagBound is how long a record-set write may take, from
// its 200 at commit, until the node answers it in DNS while it is sent ten
// signed UPDATEs a second (D405). The node prepares one publication at a
// time from its whole image; it measured well under a second without the
// race detector and under three seconds with it on a loaded machine (see
// the PUBLISH-STARVATION evidence), so the bound leaves room for a slow
// runner while still failing a node that stops publishing.
const mountedPublicationLagBound = 10 * time.Second

// mountedMinCommittedRate is the least rate of committed signed UPDATEs, a
// second, at which the load proves anything: below it the API commands are
// hardly ever superseded. It measured about one a second (151 in 146 s)
// with ten sent a second, so the floor is half of that.
const mountedMinCommittedRate = 0.5

// A ready node that is sent a steady stream of signed UPDATEs (ten sent a
// second, of which at least mountedMinCommittedRate commit, each followed by
// the node's own zone status work) serves its operators
// meanwhile: 200 sign-ins, 200 record-set writes and 200 reads of the
// written object, each sent exactly once and none retried, all succeed
// (D399, D405), and each write is answered in DNS within
// mountedPublicationLagBound of its commit. No refusal is tolerated: not at
// capture (generation_pending), not as superseded, not after a committed
// session (authentication operation unavailable).
//
// Before D405 the node's preparation time grew with every UPDATE (each
// content commit left the whole previous zone content behind as unreferenced
// chunks in the replicated image, all of which every publication read,
// checked and decoded), and every Wake after a commit canceled the
// preparation in progress, so the node fell minutes behind under the race
// detector and refused requests at capture.
func TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates(t *testing.T) {
	if raceEnabled {
		// Under the race detector one publication of this node takes one to
		// three seconds on a loaded machine (the instrumented SQLite driver and
		// the per-owner image validation, PUBLISH-STARVATION evidence), longer
		// than the two-second capture wait, so reads and sign-ins are still
		// refused at capture there; that limit is open (D405). hack/go-test.py
		// runs this test without the race detector (NORACE_TESTS).
		t.Skip("one publication outlasts the capture wait under the race detector; runs without it")
	}
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
	defer func() {
		select {
		case <-done:
		default:
			close(done)
		}
		updates.Wait()
	}()
	time.Sleep(time.Second)

	const count = 200
	counts := map[mountedRefusal]int{}
	var failures []string
	record := func(what string, i, status int, raw []byte, err error, ok ...int) bool {
		class := classifyMounted(status, raw, err, ok...)
		counts[class]++
		if class != mountedAdmitted {
			failures = append(failures, fmt.Sprintf("%s %d (%s): status %d err %v: %s", what, i, class, status, err, raw))
		}
		return class == mountedAdmitted
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := &http.Client{Jar: jar, Transport: &http.Transport{}, Timeout: 20 * time.Second}
	defer writer.CloseIdleConnections()
	var info api.SessionInfo
	status, raw, err := recaptureMountedSend(writer, base, http.MethodPost, "/auth/login", "", login)
	if !record("writer sign-in", 0, status, raw, err, http.StatusOK) || json.Unmarshal(raw, &info) != nil || info.CSRF == "" {
		t.Fatalf("writer sign-in: status %d err %v: %s", status, err, raw)
	}
	resolver := &dns.Client{Timeout: 200 * time.Millisecond}
	// served waits until name answers address and returns how long that took
	// since committed, the moment the write's 200 came back.
	served := func(name, address string, committed time.Time) (time.Duration, bool) {
		for time.Since(committed) < time.Minute {
			query := new(dns.Msg)
			query.SetQuestion(name, dns.TypeA)
			if answer, _, err := resolver.Exchange(query, cfg.Listeners.DNS[0].Address); err == nil && answer != nil && answer.Rcode == dns.RcodeSuccess && len(answer.Answer) == 1 {
				if rr, ok := answer.Answer[0].(*dns.A); ok && rr.A.String() == address {
					return time.Since(committed), true
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		return time.Minute, false
	}
	signIns, writes, reads := 0, 0, 0
	var lags []time.Duration
	began := time.Now()
	for i := range count {
		client := &http.Client{Transport: &http.Transport{}, Timeout: 20 * time.Second}
		status, raw, err := recaptureMountedSend(client, base, http.MethodPost, "/auth/login", "", login)
		client.CloseIdleConnections()
		if record("sign-in", i, status, raw, err, http.StatusOK) {
			signIns++
		}
		// Five record sets replaced in turn keep the zone's size constant.
		name := fmt.Sprintf("w%d", i%5)
		address := fmt.Sprintf("192.0.2.%d", i%250+1)
		body := public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: "kudurru.io/v1alpha1", Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "load"}, Endpoints: []public.Endpoint{{DNSName: name + ".load.test.", RecordType: "A", Targets: []string{address}}}}}
		status, raw, err = recaptureMountedSend(writer, base, http.MethodPut, "/objects/dnsrecordsets/default/"+name, info.CSRF, body)
		// The lag is measured from here: the read below waits for this
		// write's publication too, and that wait belongs to the lag.
		committedAt := time.Now()
		if !record("record-set write", i, status, raw, err, http.StatusOK, http.StatusCreated) {
			continue
		}
		writes++
		status, raw, err = recaptureMountedSend(writer, base, http.MethodGet, "/objects/dnsrecordsets/default/"+name, "", nil)
		if record("record-set read", i, status, raw, err, http.StatusOK) {
			reads++
		}
		lag, ok := served(name+".load.test.", address, committedAt)
		if !ok {
			failures = append(failures, fmt.Sprintf("record-set write %d: %s was not answered with %s for a minute", i, name, address))
		}
		lags = append(lags, lag)
	}
	elapsed := time.Since(began)
	close(done)
	updates.Wait()
	slices.Sort(lags)
	var median, worst time.Duration
	if len(lags) > 0 {
		median, worst = lags[len(lags)/2], lags[len(lags)-1]
	}
	mu.Lock()
	t.Logf("in %s: signed UPDATEs sent %d, rcodes %v; of %d sign-ins, writes and reads admitted %d, %d and %d; refused as superseded %d, at capture %d, sign-in unacknowledged %d, other %d; write to DNS answer median %s, max %s",
		elapsed.Round(time.Millisecond), sent.Load(), rcodes, count, signIns, writes, reads, counts[mountedSuperseded], counts[mountedAtCapture], counts[mountedUnacknowledged], counts[mountedOther], median.Round(time.Millisecond), worst.Round(time.Millisecond))
	committed := rcodes[dns.RcodeSuccess]
	mu.Unlock()
	// Ten UPDATEs are sent a second; about one a second commits on the
	// reference runner (the rest answer SERVFAIL: an UPDATE's ingress check
	// needs a current publication and is not recaptured, D399). The proof
	// needs a real stream of commits superseding the API commands, so the
	// committed rate must reach mountedMinCommittedRate.
	rate := float64(committed) / elapsed.Seconds()
	t.Logf("signed UPDATEs committed %d, %.2f a second", committed, rate)
	for _, failure := range failures {
		t.Error(failure)
	}
	if worst > mountedPublicationLagBound {
		t.Errorf("a write was answered in DNS %s after its commit; the bound is %s", worst.Round(time.Millisecond), mountedPublicationLagBound)
	}
	if committed == 0 || rate < mountedMinCommittedRate {
		t.Fatalf("signed UPDATEs committed %d during the load, %.2f a second; the proof needs at least %.1f a second", committed, rate, mountedMinCommittedRate)
	}
}
