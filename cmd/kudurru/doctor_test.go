package main

import (
	"io"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorConfig writes a server config whose listeners use free loopback
// ports and whose dataDir is a temp directory.
func doctorConfig(t *testing.T, dataDir string, busy net.Listener) string {
	t.Helper()
	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		return l.Addr().String()
	}
	cfg := "node:\n  name: doc\n  dataDir: " + dataDir + "\nlisteners:\n  dns:\n  - address: " + free() + "\n    protocols: [udp, tcp]\n" +
		"  api: {address: " + busy.Addr().String() + "}\n  ops: {address: " + free() + "}\n  metrics: {address: " + free() + "}\n  internal: {address: " + free() + "}\n"
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorChecksTheServer(t *testing.T) {
	h := loggedIn(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	data := t.TempDir()
	if err := os.Chmod(data, 0o750); err != nil { //nolint:gosec // G302: the mode doctor expects of a dataDir
		t.Fatal(err)
	}
	cfg := doctorConfig(t, data, busy)

	out, _, err := h.run("--config", cfg, "doctor")
	// The policy diagnosis in the fake is an error, so doctor fails.
	if err == nil || !strings.Contains(err.Error(), "1 checks failed") {
		t.Fatalf("doctor err = %v\n%s", err, out)
	}
	for _, want := range []string{
		"config: OK (" + cfg + ")",
		"dataDir: OK (writable, mode 0750)",
		"api: OK SERVER (kudurru v0.1.0-test,",
		"token: OK User admin, roles admin",
		"diagnoses: FAIL 3 messages, 1 errors",
		"  ERROR NoUpstream: forward zone has no forwarders [dnszone/lab-example-com] (hint: set spec.forward.servers)",
		"  WARNING PriorityTie: same priority as policy other [policy/default/kids]",
		"  INFO ReadOnly: objects are read-only through the API (ha-static)\n",
		"/udp: OK bindable",
		"ports: OK 1 in use by the running server (" + busy.Addr().String() + "/tcp)",
		"summary: 1 failed, 0 warnings",
	} {
		if !strings.Contains(strings.ReplaceAll(out, h.api.URL(), "SERVER"), want) {
			t.Errorf("doctor output lacks %q\n%s", want, out)
		}
	}

	// A rejected token fails; --skip-ports leaves the bind checks out.
	h.env["KUDURRU_TOKEN"] = "kdr_bad_bad"
	out, _, _ = h.run("--config", cfg, "doctor", "--skip-ports")
	if !strings.Contains(out, "token: FAIL rejected (env token): unauthenticated") || strings.Contains(out, "port ") {
		t.Errorf("bad token:\n%s", out)
	}
	delete(h.env, "KUDURRU_TOKEN")

	// Without any context, an unreachable local API is a warning only.
	_ = busy.Close()
	plain := newHarness(t)
	out, _, err = plain.run("--config", cfg, "doctor", "--skip-ports")
	if err != nil || !strings.Contains(out, "api: WARN no CLI context and nothing answers at http://"+busy.Addr().String()) {
		t.Errorf("no context: %v\n%s", err, out)
	}
	// An explicit server that does not answer fails.
	out, _, err = plain.run("--config", cfg, "--server", "http://127.0.0.1:1", "doctor", "--skip-ports")
	if err == nil || !strings.Contains(out, "api: FAIL http://127.0.0.1:1 unreachable") {
		t.Errorf("explicit unreachable server: %v\n%s", err, out)
	}
}

func TestDoctorDataDirPermissions(t *testing.T) {
	h := newHarness(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	data := t.TempDir()
	if err := os.Chmod(data, 0o777); err != nil { //nolint:gosec // G302: the bad mode doctor must flag
		t.Fatal(err)
	}
	cfg := doctorConfig(t, data, busy)
	out, _, _ := h.run("--config", cfg, "doctor", "--skip-api", "--skip-ports")
	if !strings.Contains(out, "dataDir: WARN "+data+" is world-writable (mode 0777)") {
		t.Errorf("world-writable dataDir:\n%s", out)
	}
	_ = os.Chmod(data, 0o750)                                                         //nolint:gosec // G302: back to a sane dataDir mode
	if err := os.WriteFile(filepath.Join(data, "state.db"), nil, 0o644); err != nil { //nolint:gosec // G306: the bad mode doctor must flag
		t.Fatal(err)
	}
	out, _, _ = h.run("--config", cfg, "doctor", "--skip-api", "--skip-ports")
	if !strings.Contains(out, "state.db is world-readable") {
		t.Errorf("world-readable state.db:\n%s", out)
	}
}

func TestDoctorLocalTLSListener(t *testing.T) {
	api := newFakeAPI(t)
	tlsSrv := httptest.NewUnstartedServer(api.srv.Config.Handler)
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	h := newHarness(t)
	addr := strings.TrimPrefix(tlsSrv.URL, "https://")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := doctorConfig(t, t.TempDir(), l)
	_ = l.Close()
	b, _ := os.ReadFile(cfg)                                                                     //nolint:gosec // G304: the test's temp file
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(b), l.Addr().String(), addr, 1)), 0o600) //nolint:gosec // G703: the test's temp file
	out, _, _ := h.run("--config", cfg, "doctor", "--skip-ports")
	if !strings.Contains(out, "api: OK https://"+addr+" (kudurru v0.1.0-test") {
		t.Fatalf("local TLS listener not probed over https:\n%s", out)
	}
}
