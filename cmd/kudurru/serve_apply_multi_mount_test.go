package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// startMountedServe runs the actual serve command on the complete SQLite
// runtime and waits for its first complete publication. The returned function
// stops it; the test's cleanup stops it too.
func startMountedServe(t *testing.T, cfg *config.Config) func() {
	t.Helper()
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
				t.Error("multi-document mount shutdown failed", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("multi-document mount shutdown did not join")
		}
	}
	t.Cleanup(stop)
	until := time.Now().Add(30 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		select {
		case err := <-done:
			t.Fatal("multi-document mount startup ended", err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(until) {
			t.Fatal("multi-document mount never became ready")
		}
	}
	return stop
}

// mountedCLI signs the real CLI in to the mounted node with the configured
// administrator password, as an operator does.
func mountedCLI(t *testing.T, cfg *config.Config, name string) *harness {
	t.Helper()
	cli := &harness{t: t, cfgPath: filepath.Join(t.TempDir(), "cli.yaml"), env: map[string]string{}, keyring: &memKeyring{}}
	deadline := time.Now().Add(20 * time.Second)
	for {
		// A sign-in is three requests, each of them a single command that is
		// held to the publication it began on. A sign-in that meets the node's
		// follow-up work on the request before it is refused; the next attempt
		// is a new sign-in, never a replay.
		cli.stdin = "mounted-test-password-1\n"
		_, stderr, err := cli.run("login", "--server", "http://"+cfg.Listeners.API.Address, "--username", "admin", "--password-stdin", "--name", name)
		if err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("CLI sign-in failed: %v\n%s", err, stderr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cli.stdin = ""
	return cli
}

// mountedQuiet waits until the node is ready and its objects have not changed
// for a while. A request is held to the publication it began on until its
// first item, so a test that wants to observe ONE request must not start it
// while the node still publishes its own follow-up work on an earlier one.
func mountedQuiet(t *testing.T, cfg *config.Config, cli *harness) {
	t.Helper()
	read := func() (string, bool) {
		var all strings.Builder
		for _, kind := range []string{"dnszones", "dnsrecordsets", "blocklists", "clientgroups", "policies"} {
			out, _, err := cli.run("get", kind, "-A", "-o", "json")
			if err != nil {
				return "", false
			}
			all.WriteString(out)
		}
		return all.String(), offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK
	}
	deadline := time.Now().Add(time.Minute)
	last, since := "", time.Now()
	for {
		now, ready := read()
		if !ready || now != last {
			last, since = now, time.Now()
		} else if time.Since(since) >= 1500*time.Millisecond {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the mounted node never became quiet")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mountedRequest runs one CLI command that sends ONE multi-item request and
// returns its output. A request is held to the publication it began on until
// its first item, exactly as a single write is, so the node's own work between
// the request's capture and that item (a status write, a replaced provider
// resource set) refuses the whole request before anything is applied. Only
// that outcome is tried again, as a new request. A request that applied one
// item and refused another one is returned to the caller or fails the test.
func mountedRequest(t *testing.T, cfg *config.Config, cli *harness, args ...string) (string, string) {
	t.Helper()
	out, stderr, _, _ := mountedRequestWindow(t, cfg, cli, args...)
	return out, stderr
}

// mountedRequestWindow is mountedRequest that also returns when the attempt
// it returns began and ended.
func mountedRequestWindow(t *testing.T, cfg *config.Config, cli *harness, args ...string) (string, string, time.Time, time.Time) {
	t.Helper()
	const stale = "current writable serving generation unavailable"
	for attempt := 1; ; attempt++ {
		mountedQuiet(t, cfg, cli)
		began := time.Now()
		out, stderr, err := cli.run(args...)
		ended := time.Now()
		untouched := false
		var processed api.Processed
		switch {
		case err == nil && args[0] == "api" && json.Unmarshal([]byte(out), &processed) == nil:
			// The raw API command exits 0 with per-name results.
			untouched = len(processed.Processed.Success) == 0 && len(processed.Processed.Errors) > 0
			for _, item := range processed.Processed.Errors {
				untouched = untouched && strings.Contains(item.Error, stale)
			}
		case err == nil:
			mountedRequestDone(t, args, attempt)
			return out, stderr, began, ended
		case strings.Contains(err.Error(), "generation_pending"):
			untouched = true // refused at capture, before the request was read
		case !strings.Contains(out, " created") && !strings.Contains(out, " updated") && !strings.Contains(out, " unchanged"):
			refused := 0
			untouched = true
			for _, line := range strings.Split(stderr, "\n") {
				if strings.Contains(line, ": error: ") {
					refused++
					untouched = untouched && strings.Contains(line, stale)
				}
			}
			untouched = untouched && refused > 0
		}
		if !untouched {
			if err != nil {
				t.Fatalf("kudurru %s (attempt %d) did not complete in one request: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), attempt, err, out, stderr)
			}
			mountedRequestDone(t, args, attempt)
			return out, stderr, began, ended
		}
		if attempt == 5 {
			t.Fatalf("kudurru %s began five times on a publication that the node retired before the first item\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), out, stderr)
		}
		t.Logf("kudurru %s: attempt %d began on a publication that the node retired before the first item; nothing was applied", args[0], attempt)
	}
}

// mountedRequestDone logs how many whole requests that applied nothing were
// sent again, as one countable line, so that a rising rate is seen in logs.
func mountedRequestDone(t *testing.T, args []string, attempt int) {
	t.Helper()
	t.Logf("mounted request kudurru %s: repeated=%d", args[0], attempt-1)
}

func recordSetDocument(name, zone, host, address string) string {
	return fmt.Sprintf(`apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: {name: %s, namespace: default}
spec:
  zoneRef: {name: %s}
  endpoints:
    - {dnsName: %s, recordType: A, targets: [%s], recordTTL: 60}
`, name, zone, host, address)
}

const multiZoneDocument = `apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: multi}
spec: {zoneName: multi.test., type: primary, nameservers: [ns.multi.test.]}
`

func writeManifest(t *testing.T, name string, documents ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.Join(documents, "---\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// One `kudurru apply` of a file with three new record sets creates all three
// in one request on a standalone complete SQLite node, the node serves them,
// and the same command again reports three unchanged documents. One
// POST /{kind}:batchDelete then deletes all three in one request.
func TestRunServeSQLiteMultiDocumentApplyCLI(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	startMountedServe(t, cfg)
	cli := mountedCLI(t, cfg, "mounted")

	zone := writeManifest(t, "zone.yaml", multiZoneDocument)
	if out, _ := mountedRequest(t, cfg, cli, "apply", "-f", zone); !strings.Contains(out, "dnszone/multi created") {
		t.Fatalf("zone apply failed:\n%s", out)
	}
	three := writeManifest(t, "three.yaml",
		recordSetDocument("x0", "multi", "x0.multi.test.", "198.51.100.1"),
		recordSetDocument("x1", "multi", "x1.multi.test.", "198.51.100.2"),
		recordSetDocument("x2", "multi", "x2.multi.test.", "198.51.100.3"))
	out, _ := mountedRequest(t, cfg, cli, "apply", "-f", three)
	for _, name := range []string{"x0", "x1", "x2"} {
		if !strings.Contains(out, "dnsrecordset/default/"+name+" created") {
			t.Fatalf("document %s was not created in the one request:\n%s", name, out)
		}
	}
	awaitMountedDNS(t, cfg, "x0.multi.test.", "198.51.100.1")
	awaitMountedDNS(t, cfg, "x1.multi.test.", "198.51.100.2")
	awaitMountedDNS(t, cfg, "x2.multi.test.", "198.51.100.3")
	if out, _ = mountedRequest(t, cfg, cli, "apply", "-f", three); strings.Count(out, " unchanged") != 3 {
		t.Fatalf("second identical apply did not report three unchanged documents:\n%s", out)
	}

	// The raw API command prints the per-name result and exits 0 also when a
	// name failed, so the result itself is the assertion.
	out, stderr := mountedRequest(t, cfg, cli, "api", "POST", "/dnsrecordsets:batchDelete", "--data", `{"names":["x0","x1","x2"]}`)
	var deleted api.Processed
	if json.Unmarshal([]byte(out), &deleted) != nil || len(deleted.Processed.Success) != 3 || len(deleted.Processed.Errors) != 0 {
		t.Fatalf("one batch delete of three record sets did not delete all of them:\nstdout:\n%s\nstderr:\n%s", out, stderr)
	}
	for _, name := range []string{"x0", "x1", "x2"} {
		awaitMountedDNSAbsent(t, cfg, name+".multi.test.")
		if _, _, err := cli.run("get", "dnsrecordsets", name); err == nil {
			t.Fatalf("record set %s survived the batch delete", name)
		}
	}
}

// `kudurru apply -f a -f b -f c` applies three files in one run, and a
// `kudurru restore` of that node's backup into a new node applies every
// document in one run: the node's bootstrap objects are taken over and the
// zone and its record sets are created and served.
func TestRunServeSQLiteMultiFileApplyAndRestoreIntoNewNode(t *testing.T) {
	source, _ := mountedTestConfig(t)
	stop := startMountedServe(t, source)
	cli := mountedCLI(t, source, "source")
	zone := writeManifest(t, "zone.yaml", multiZoneDocument)
	mountedRequest(t, source, cli, "apply", "-f", zone)
	files := []string{
		writeManifest(t, "a.yaml", recordSetDocument("y0", "multi", "y0.multi.test.", "198.51.100.10")),
		writeManifest(t, "b.yaml", recordSetDocument("y1", "multi", "y1.multi.test.", "198.51.100.11")),
		writeManifest(t, "c.yaml", recordSetDocument("y2", "multi", "y2.multi.test.", "198.51.100.12")),
	}
	out, _ := mountedRequest(t, source, cli, "apply", "-f", files[0], "-f", files[1], "-f", files[2])
	if strings.Count(out, " created") != 3 {
		t.Fatalf("one apply of three files did not create three record sets:\n%s", out)
	}
	awaitMountedDNS(t, source, "y2.multi.test.", "198.51.100.12")
	mountedQuiet(t, source, cli)
	backup := filepath.Join(t.TempDir(), "backup.yaml")
	if out, stderr, err := cli.run("backup", "--exclude-managed", "--out", backup); err != nil {
		t.Fatalf("backup failed: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	raw, err := os.ReadFile(backup) // #nosec G304 -- the backup this test wrote in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	documents := strings.Count(string(raw), "\n---\n") + strings.Count(string(raw[:4]), "---\n")
	if documents < 5 || !strings.Contains(string(raw), "name: y2") {
		t.Fatalf("the backup does not hold the zone, its record sets and the node's own objects: %d documents", documents)
	}
	stop()

	target, _ := mountedTestConfig(t)
	startMountedServe(t, target)
	restored := mountedCLI(t, target, "target")
	out, stderr := mountedRequest(t, target, restored, "restore", "-f", backup)
	if strings.Contains(stderr, "error") {
		t.Fatalf("one restore into a new node did not apply every document:\nstdout:\n%s\nstderr:\n%s", out, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != documents || !strings.Contains(out, "dnszone/multi created") || strings.Count(out, "dnsrecordset/default/y") != 3 {
		t.Fatalf("the restore did not report one applied result for each of the %d documents:\n%s", documents, out)
	}
	for _, line := range lines {
		if !strings.HasSuffix(line, " created") && !strings.HasSuffix(line, " updated") && !strings.HasSuffix(line, " unchanged") {
			t.Fatalf("a restored document was not applied: %q", line)
		}
	}
	awaitMountedDNS(t, target, "y0.multi.test.", "198.51.100.10")
	awaitMountedDNS(t, target, "y1.multi.test.", "198.51.100.11")
	awaitMountedDNS(t, target, "y2.multi.test.", "198.51.100.12")
	if out, _ = mountedRequest(t, target, restored, "restore", "-f", backup); strings.Count(out, " unchanged") != documents {
		t.Fatalf("a second restore did not report every document unchanged:\n%s", out)
	}
}

// One long apply on a standalone complete SQLite node keeps DNS and other
// callers served (D378): an existing record of the node's own zone is
// answered throughout, from the publication that is served, and a second
// signed-in CLI still reads, because a waiting read makes the request let the
// node publish at its next item. Before, DNS bound local answers to the latest
// commit and to the provider resource set, and a read waited for the next work
// budget: an apply of 120 documents left the record unanswered for 80 s and
// refused 41 of 43 reads. The CLI's own default timeout of apply is used.
func TestRunServeSQLiteLongApplyKeepsServingDNSAndReads(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	startMountedServe(t, cfg)
	cli := mountedCLI(t, cfg, "writer")
	reader := mountedCLI(t, cfg, "reader")
	mountedRequest(t, cfg, cli, "apply", "-f", writeManifest(t, "zone.yaml", multiZoneDocument))
	mountedRequest(t, cfg, cli, "apply", "-f", writeManifest(t, "probe.yaml", recordSetDocument("probe", "multi", "probe.multi.test.", "198.51.100.200")))
	awaitMountedDNS(t, cfg, "probe.multi.test.", "198.51.100.200")

	const count = 120
	documents := make([]string, 0, count)
	for i := range count {
		documents = append(documents, recordSetDocument(fmt.Sprintf("long%03d", i), "multi", fmt.Sprintf("long%03d.multi.test.", i), fmt.Sprintf("198.51.100.%d", i%200+1)))
	}
	long := writeManifest(t, "long.yaml", documents...)

	var mu sync.Mutex
	var answered, servfail, read, failedRead []time.Time
	// missed holds every probe that got no correct answer, with what it got,
	// so that a gap is explained in the failure message.
	type miss struct {
		at   time.Time
		what string
	}
	var missed []miss
	var readError string
	stop := make(chan struct{})
	var probes sync.WaitGroup
	probes.Go(func() {
		client := &dns.Client{Timeout: 200 * time.Millisecond}
		for {
			select {
			case <-stop:
				return
			default:
			}
			query := new(dns.Msg)
			query.SetQuestion("probe.multi.test.", dns.TypeA)
			sent := time.Now()
			answer, _, err := client.Exchange(query, cfg.Listeners.DNS[0].Address)
			mu.Lock()
			switch {
			case err != nil:
				missed = append(missed, miss{sent, err.Error()})
			case answer.Rcode == dns.RcodeServerFailure:
				servfail = append(servfail, time.Now())
				missed = append(missed, miss{sent, "SERVFAIL"})
			case answer.Rcode == dns.RcodeSuccess && len(answer.Answer) == 1 && probeAddress(answer.Answer[0]) == "198.51.100.200":
				answered = append(answered, time.Now())
			default:
				missed = append(missed, miss{sent, dns.RcodeToString[answer.Rcode] + " " + fmt.Sprint(answer.Answer)})
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
	})
	probes.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			_, stderr, err := reader.run("get", "dnszones", "-o", "name")
			mu.Lock()
			if err == nil {
				read = append(read, time.Now())
			} else {
				failedRead = append(failedRead, time.Now())
				readError = fmt.Sprintf("%v: %s", err, strings.TrimSpace(stderr))
			}
			mu.Unlock()
		}
	})
	out, stderr, began, ended := mountedRequestWindow(t, cfg, cli, "apply", "-f", long)
	close(stop)
	probes.Wait()
	if created := strings.Count(out, " created"); created != count {
		t.Fatalf("one apply of %d new record sets created %d:\nstdout:\n%s\nstderr:\n%s", count, created, out, stderr)
	}
	within := func(times []time.Time) []time.Time {
		var in []time.Time
		for _, at := range times {
			if at.After(began) && at.Before(ended) {
				in = append(in, at)
			}
		}
		return in
	}
	mu.Lock()
	answers, servfails, reads, failed := within(answered), len(within(servfail)), within(read), len(within(failedRead))
	mu.Unlock()
	edges := append(append([]time.Time{began}, answers...), ended)
	slices.SortFunc(edges, func(a, b time.Time) int { return a.Compare(b) })
	var gap time.Duration
	var gapFrom time.Time
	for i := 1; i < len(edges); i++ {
		if d := edges[i].Sub(edges[i-1]); d > gap {
			gap, gapFrom = d, edges[i-1]
		}
	}
	t.Logf("apply of %d documents took %s: %d correct DNS answers, %d SERVFAIL, longest time without one %s, %d reads ok, %d reads failed", count, ended.Sub(began), len(answers), servfails, gap, len(reads), failed)
	// The zone answers its committed data across every status write of a new
	// serial that follows each commit (D378 and the status-write row): a
	// publication that lands while the zone's lifecycle receipt still names
	// the DNSZone's previous resourceVersion serves the same head. Before
	// that, such a publication answered the zone SERVFAIL until the
	// lifecycle caught up, for up to 0.8 s, which left the gap bound below
	// with little margin.
	if servfails != 0 {
		t.Fatalf("the zone answered SERVFAIL %d times for an existing record during one request", servfails)
	}
	// DNS answers the served publication throughout (D378): a commit that is
	// not published yet, and a replaced provider resource set, withhold
	// nothing. The probe asks every 50 ms with a 200 ms timeout, so a gap is
	// usually about 55 ms and one dropped query makes it about 0.3 s. One
	// retained run (CORE-OPEN3 run05) had a 1.39 s gap without any SERVFAIL
	// whose cause is not known; the message below says what each probe in
	// such a gap got.
	if gap > time.Second {
		mu.Lock()
		var inGap []string
		for _, m := range missed {
			if !m.at.Before(gapFrom) && m.at.Before(gapFrom.Add(gap)) {
				inGap = append(inGap, fmt.Sprintf("+%s %s", m.at.Sub(gapFrom).Round(time.Millisecond), m.what))
			}
		}
		mu.Unlock()
		t.Fatalf("the node left an existing record unanswered for %s, %s after the request began, during one request of %s; probes in the gap: %q", gap, gapFrom.Sub(began).Round(time.Millisecond), ended.Sub(began), inGap)
	}
	// A waiting read makes the request let the node publish at its next item,
	// so a read is served within its two-second capture wait whenever the
	// node publishes within it, as it does without the race detector (one
	// publication of these 120 record sets takes 0.3 to 0.5 s there). Under
	// the race detector on a loaded machine one publication takes several
	// seconds, longer than the capture wait, and most reads that wait for one
	// are refused with generation_pending; the request still lets some reads
	// through, which the last check requires.
	// `make test` (hack/go-test.py, NORACE_TESTS) runs this test a second
	// time without the race detector, so CI asserts the strict bound.
	if !raceEnabled && failed > 0 {
		t.Fatalf("%d reads of a second caller were refused during one request (%d answered); last: %s", failed, len(reads), readError)
	}
	if elapsed := ended.Sub(began); elapsed > 5*defaultItemPace.work && len(reads) == 0 {
		t.Fatalf("no read of a second caller was answered during the %s of one request; last refusal: %s", elapsed, readError)
	}
}

func probeAddress(rr dns.RR) string {
	if a, ok := rr.(*dns.A); ok {
		return a.A.String()
	}
	return ""
}
