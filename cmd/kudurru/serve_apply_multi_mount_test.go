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

	mountedQuiet(t, cfg, cli)
	zone := writeManifest(t, "zone.yaml", multiZoneDocument)
	if out, stderr, err := cli.run("apply", "-f", zone); err != nil || !strings.Contains(out, "dnszone/multi created") {
		t.Fatalf("zone apply failed: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	mountedQuiet(t, cfg, cli)
	three := writeManifest(t, "three.yaml",
		recordSetDocument("x0", "multi", "x0.multi.test.", "198.51.100.1"),
		recordSetDocument("x1", "multi", "x1.multi.test.", "198.51.100.2"),
		recordSetDocument("x2", "multi", "x2.multi.test.", "198.51.100.3"))
	out, stderr, err := cli.run("apply", "-f", three)
	if err != nil {
		t.Fatalf("one apply of three new record sets did not complete: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	for _, name := range []string{"x0", "x1", "x2"} {
		if !strings.Contains(out, "dnsrecordset/default/"+name+" created") {
			t.Fatalf("document %s was not created in the one request:\n%s", name, out)
		}
	}
	awaitMountedDNS(t, cfg, "x0.multi.test.", "198.51.100.1")
	awaitMountedDNS(t, cfg, "x1.multi.test.", "198.51.100.2")
	awaitMountedDNS(t, cfg, "x2.multi.test.", "198.51.100.3")
	mountedQuiet(t, cfg, cli)
	out, stderr, err = cli.run("apply", "-f", three)
	if err != nil || strings.Count(out, " unchanged") != 3 {
		t.Fatalf("second identical apply did not report three unchanged documents: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}

	mountedQuiet(t, cfg, cli)
	out, stderr, err = cli.run("api", "POST", "/dnsrecordsets:batchDelete", "--data", `{"names":["x0","x1","x2"]}`)
	var deleted api.Processed
	if err != nil || json.Unmarshal([]byte(out), &deleted) != nil || len(deleted.Processed.Success) != 3 || len(deleted.Processed.Errors) != 0 {
		t.Fatalf("one batch delete of three record sets did not delete all of them: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
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
	mountedQuiet(t, source, cli)
	zone := writeManifest(t, "zone.yaml", multiZoneDocument)
	if out, stderr, err := cli.run("apply", "-f", zone); err != nil {
		t.Fatalf("zone apply failed: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	mountedQuiet(t, source, cli)
	files := []string{
		writeManifest(t, "a.yaml", recordSetDocument("y0", "multi", "y0.multi.test.", "198.51.100.10")),
		writeManifest(t, "b.yaml", recordSetDocument("y1", "multi", "y1.multi.test.", "198.51.100.11")),
		writeManifest(t, "c.yaml", recordSetDocument("y2", "multi", "y2.multi.test.", "198.51.100.12")),
	}
	out, stderr, err := cli.run("apply", "-f", files[0], "-f", files[1], "-f", files[2])
	if err != nil || strings.Count(out, " created") != 3 {
		t.Fatalf("one apply of three files did not create three record sets: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	awaitMountedDNS(t, source, "y2.multi.test.", "198.51.100.12")
	mountedQuiet(t, source, cli)
	backup := filepath.Join(t.TempDir(), "backup.yaml")
	if out, stderr, err := cli.run("backup", "--exclude-managed", "--out", backup); err != nil {
		t.Fatalf("backup failed: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	raw, err := os.ReadFile(backup)
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
	mountedQuiet(t, target, restored)
	out, stderr, err = restored.run("restore", "-f", backup)
	if err != nil || strings.Contains(stderr, "error") {
		t.Fatalf("one restore into a new node did not apply every document: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
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
	mountedQuiet(t, target, restored)
	out, stderr, err = restored.run("restore", "-f", backup)
	if err != nil || strings.Count(out, " unchanged") != documents {
		t.Fatalf("a second restore did not report every document unchanged: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
}
