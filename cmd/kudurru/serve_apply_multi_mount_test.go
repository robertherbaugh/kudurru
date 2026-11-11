package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	const stale = "current writable serving generation unavailable"
	for attempt := 1; ; attempt++ {
		mountedQuiet(t, cfg, cli)
		out, stderr, err := cli.run(args...)
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
			return out, stderr
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
			return out, stderr
		}
		if attempt == 5 {
			t.Fatalf("kudurru %s began five times on a publication that the node retired before the first item\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), out, stderr)
		}
		t.Logf("kudurru %s: attempt %d began on a publication that the node retired before the first item; nothing was applied", args[0], attempt)
	}
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
