package e2e

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	_ "modernc.org/sqlite" // read querylog.db after the node stopped
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// probe is one readiness observation.
type probe struct {
	at   time.Time
	code int // 0 when the request failed
}

// pollReady polls GET /healthz/ready as fast as it can until stop closes.
func pollReady(addr string, stop <-chan struct{}) []probe {
	hc := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	var out []probe
	for {
		select {
		case <-stop:
			return out
		default:
		}
		p := probe{at: time.Now()}
		if resp, err := hc.Get("http://" + addr + "/healthz/ready"); err == nil {
			p.code = resp.StatusCode
			_ = resp.Body.Close()
		}
		out = append(out, p)
		time.Sleep(time.Millisecond)
	}
}

// TestSIGTERMLameduck sends SIGTERM while a slow forwarded query is in
// flight: readiness must turn 503 before the listeners close, and the
// in-flight query must still be answered.
func TestSIGTERMLameduck(t *testing.T) {
	kudurrutest.RequireE2E(t)
	up := kudurrutest.StartUpstream(t, map[string]string{"slow.example.": "198.51.100.7", "fast.example.": "198.51.100.8"})
	up.SetSlow("slow.example.", 1500*time.Millisecond)
	spec, cfg := kudurrutest.NewNodeSpec(t, t.TempDir(), "lameduck")
	cfg["upstreams"].(map[string]any)["groups"] = map[string]any{
		"default": map[string]any{"servers": []any{up.Addr}, "strategy": "strict", "timeout": "5s"},
	}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	n := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
	// Readiness can precede the first live snapshot (E2E-5).
	if !kudurrutest.Eventually(5*time.Second, 5*time.Millisecond, func() bool {
		r, err := exchange(spec.Ports.DNS, "fast.example.", dns.TypeA, false)
		return err == nil && firstA(r) == "198.51.100.8"
	}) {
		t.Fatal("no forwarding before shutdown")
	}

	stop := make(chan struct{})
	var probes []probe
	var wg sync.WaitGroup
	wg.Go(func() { probes = pollReady(spec.Ports.Ops, stop) })

	type result struct {
		r   *dns.Msg
		err error
		at  time.Time
	}
	inflight := make(chan result, 2)
	for _, tcp := range []bool{false, true} {
		go func() {
			r, err := exchange(spec.Ports.DNS, "slow.example.", dns.TypeA, tcp)
			inflight <- result{r, err, time.Now()}
		}()
	}
	// Let the queries reach the upstream (the two identical questions may
	// be coalesced into one upstream query), then SIGTERM.
	if !kudurrutest.Eventually(3*time.Second, 5*time.Millisecond, func() bool { return up.Queries() >= 2 }) {
		t.Fatalf("slow queries did not reach the upstream (%d)", up.Queries())
	}
	sigAt := time.Now()
	if err := n.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code, err := n.Wait(20 * time.Second)
	exitAt := time.Now()
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("exit code %d after SIGTERM, want 0", code)
	}
	t.Logf("timing: SIGTERM -> exit in %s", exitAt.Sub(sigAt).Round(time.Millisecond))

	t.Run("InFlightQueriesComplete", func(t *testing.T) {
		for range 2 {
			res := <-inflight
			if res.err != nil || firstA(res.r) != "198.51.100.7" {
				t.Errorf("in-flight query lost on shutdown: %v %v", res.r, res.err)
				continue
			}
			t.Logf("in-flight answer %s after SIGTERM", res.at.Sub(sigAt).Round(time.Millisecond))
		}
	})

	t.Run("ReadinessFlipsBeforeListenersClose", func(t *testing.T) {
		var first503, firstDown time.Time
		var after []int
		for _, p := range probes {
			if p.at.Before(sigAt) {
				continue
			}
			switch {
			case p.code == http.StatusServiceUnavailable && first503.IsZero():
				first503 = p.at
			case p.code == 0 && firstDown.IsZero():
				firstDown = p.at
			}
			if !firstDown.IsZero() && p.code != 0 {
				after = append(after, p.code)
			}
		}
		for _, p := range probes {
			if !first503.IsZero() && p.at.After(first503) && p.code == http.StatusOK {
				t.Errorf("readiness went back to 200 after 503")
				break
			}
		}
		if len(after) > 0 {
			t.Errorf("ops listener answered %v after it had closed", after)
		}
		if first503.IsZero() {
			t.Fatal("readiness never answered 503 after SIGTERM")
		}
		window := firstDown.Sub(first503)
		t.Logf("timing: 503 after %s, ops closed after %s (lameduck window %s)", first503.Sub(sigAt).Round(time.Microsecond), firstDown.Sub(sigAt).Round(time.Microsecond), window.Round(time.Microsecond))
		if window < time.Second {
			t.Errorf("lameduck window %s, want at least 1 s", window)
		}
	})
}

// queryLogRows returns the logged query names in id order.
func queryLogRows(t *testing.T, dataDir string) (ids []int64, names []string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "querylog.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT q.id, d.name FROM query q JOIN dict_domain d ON d.id = q.domain ORDER BY q.id`)
	if err != nil {
		t.Fatalf("querylog.db: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		ids, names = append(ids, id), append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids, names
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(strings.TrimSuffix(n, "."), strings.TrimSuffix(want, ".")) {
			return true
		}
	}
	return false
}

// TestRestartKeepsState restarts a node over its existing state.db,
// querylog.db and local.db: file objects stay, the zone serial continues,
// the query log keeps its rows and ids, and the response cache is restored
// so that a cached name still resolves with the upstream gone.
func TestRestartKeepsState(t *testing.T) {
	kudurrutest.RequireE2E(t)
	dir := t.TempDir()
	up := kudurrutest.StartUpstream(t, map[string]string{"cached.example.": "198.51.100.20"})
	spec, cfg := kudurrutest.NewNodeSpec(t, dir, "restart")
	objDir := filepath.Join(dir, "objects.d")
	if err := os.MkdirAll(objDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeObjects := func(records map[string]string) {
		t.Helper()
		docs := zoneObjects(records)
		var b strings.Builder
		for _, d := range docs {
			out, err := yaml.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString("---\n")
			b.Write(out)
		}
		if err := os.WriteFile(filepath.Join(objDir, "zone.yaml"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeObjects(map[string]string{"ns1": "127.0.0.1", "host": "192.0.2.1"})
	cfg["objectsDir"] = objDir
	cfg["upstreams"].(map[string]any)["groups"] = map[string]any{
		"default": map[string]any{"servers": []any{up.Addr}, "strategy": "strict", "timeout": "2s"},
	}
	// exclude.sudn is on by default and would drop e2e.test (RFC 6761).
	cfg["querylog"] = map[string]any{"enabled": true, "sink": "sqlite", "exclude": map[string]any{"sudn": false}}
	kudurrutest.WriteConfig(t, spec.Config, cfg)

	// Run 1.
	n1 := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
	if !strings.Contains(n1.Log(), "first boot: seeded defaults") {
		t.Error("run 1 is not a first boot")
	}
	serial := func() uint32 {
		r, err := exchange(spec.Ports.DNS, "e2e.test.", dns.TypeSOA, false)
		if err != nil || len(r.Answer) == 0 {
			return 0
		}
		return r.Answer[0].(*dns.SOA).Serial
	}
	var serial1 uint32
	if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool { serial1 = serial(); return serial1 > 1 }) {
		t.Fatalf("no stamped serial for e2e.test (%d)", serial1)
	}
	if r, err := exchange(spec.Ports.DNS, "host.e2e.test.", dns.TypeA, false); err != nil || firstA(r) != "192.0.2.1" {
		t.Fatalf("authoritative answer: %v %v", r, err)
	}
	if r, err := exchange(spec.Ports.DNS, "cached.example.", dns.TypeA, false); err != nil || firstA(r) != "198.51.100.20" {
		t.Fatalf("forwarded answer: %v %v", r, err)
	}
	keyFile := filepath.Join(spec.DataDir, "keys", "encryption.key")
	key1, err := os.ReadFile(keyFile) //nolint:gosec // test data directory
	if err != nil {
		t.Fatalf("generated encryption key: %v", err)
	}
	n1.Stop(20 * time.Second)
	if !strings.Contains(n1.Log(), "cache dump saved") {
		t.Errorf("run 1 did not save the cache dump")
	}
	ids1, names1 := queryLogRows(t, spec.DataDir)
	if !containsName(names1, "host.e2e.test") || !containsName(names1, "cached.example") {
		t.Errorf("run 1 query log lacks the queries: %v", names1)
	}

	// Run 2: same files, upstream gone.
	up.Stop()
	start := time.Now()
	n2 := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
	t.Logf("timing: restart ready in %s", time.Since(start).Round(time.Millisecond))
	// Readiness can precede the first live zone snapshot (E2E-5,
	// TestReadyMeansServing); wait for the gate before asserting answers.
	if !kudurrutest.Eventually(5*time.Second, 5*time.Millisecond, func() bool {
		r, err := exchange(spec.Ports.DNS, "e2e.test.", dns.TypeSOA, false)
		return err == nil && r.Rcode == dns.RcodeSuccess
	}) {
		t.Fatal("e2e.test not served after the restart")
	}

	t.Run("NoFirstBoot", func(t *testing.T) {
		log := n2.Log()
		for _, s := range []string{"first boot: seeded defaults", "created the first-boot admin", "bootstrap: created"} {
			if strings.Contains(log, s) {
				t.Errorf("run 2 logged %q: state.db was not reused", s)
			}
		}
		key2, err := os.ReadFile(keyFile) //nolint:gosec // test data directory
		if err != nil || string(key2) != string(key1) {
			t.Errorf("encryption key changed across the restart (%v)", err)
		}
		if strings.Contains(log, "generated the encryption key") {
			t.Error("run 2 logged \"generated the encryption key\" although it reused the key file")
		}
	})
	t.Run("SerialContinuity", func(t *testing.T) {
		if s := serial(); s != serial1 {
			t.Errorf("serial after restart %d, want %d (unchanged content)", s, serial1)
		}
		if r, err := exchange(spec.Ports.DNS, "host.e2e.test.", dns.TypeA, false); err != nil || firstA(r) != "192.0.2.1" {
			t.Errorf("authoritative answer after restart: %v %v", r, err)
		}
	})
	t.Run("CacheRestored", func(t *testing.T) {
		if !strings.Contains(n2.Log(), "cache restored") {
			t.Errorf("run 2 did not log a cache restore")
		}
		r, err := exchange(spec.Ports.DNS, "cached.example.", dns.TypeA, false)
		if err != nil || firstA(r) != "198.51.100.20" {
			t.Errorf("cached.example not answered from the restored cache with the upstream gone: %v %v", r, err)
		}
	})
	if _, err := exchange(spec.Ports.DNS, "host.e2e.test.", dns.TypeA, true); err != nil {
		t.Fatal(err)
	}
	n2.Stop(20 * time.Second)

	t.Run("QueryLogContinuity", func(t *testing.T) {
		ids2, names2 := queryLogRows(t, spec.DataDir)
		if len(ids2) <= len(ids1) {
			t.Fatalf("query log rows: %d after run 2, %d after run 1", len(ids2), len(ids1))
		}
		for i := range ids1 {
			if ids2[i] != ids1[i] || names2[i] != names1[i] {
				t.Fatalf("run 1 row %d changed: %d %s -> %d %s", i, ids1[i], names1[i], ids2[i], names2[i])
			}
		}
		if ids2[len(ids1)] <= ids1[len(ids1)-1] {
			t.Errorf("ids restarted: %v", ids2)
		}
	})

	// Run 3: the zone content changed while the node was down.
	writeObjects(map[string]string{"ns1": "127.0.0.1", "host": "192.0.2.1", "added": "192.0.2.2"})
	n3 := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
	defer n3.Stop(20 * time.Second)
	t.Run("SerialAdvances", func(t *testing.T) {
		var s uint32
		if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool { s = serial(); return s > serial1 }) {
			t.Fatalf("serial after a change while down: %d, want > %d", s, serial1)
		}
		if r, err := exchange(spec.Ports.DNS, "added.e2e.test.", dns.TypeA, false); err != nil || firstA(r) != "192.0.2.2" {
			t.Errorf("new record: %v %v", r, err)
		}
	})
}

// runExpectFailure runs the binary with args and env and returns the exit
// code, the combined output and the duration.
func runExpectFailure(t *testing.T, env []string, args ...string) (int, string, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, kudurruBin, args...) //nolint:gosec // test binary
	cmd.Env = append(os.Environ(), env...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	d := time.Since(start)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out), d
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out), d
	default:
		t.Fatalf("run kudurru: %v", err)
	}
	return 0, "", 0
}

// TestConfigValidationFails checks that invalid configurations exit
// non-zero, quickly, with a message naming the offending key.
func TestConfigValidationFails(t *testing.T) {
	kudurrutest.RequireE2E(t)
	dir := t.TempDir()
	spec, _ := kudurrutest.NewNodeSpec(t, dir, "invalid")
	secretMissing := filepath.Join(dir, "does-not-exist.secret")
	cases := []struct {
		name   string
		mutate func(cfg map[string]any)
		raw    string   // replaces the YAML when set
		env    []string // extra environment
		want   string
	}{
		{name: "unknown store driver", mutate: func(c map[string]any) { c["store"] = map[string]any{"driver": "etcd"} }, want: `store.driver: unknown driver "etcd"`},
		{name: "unknown upstream strategy", mutate: func(c map[string]any) {
			c["upstreams"].(map[string]any)["groups"] = map[string]any{"default": map[string]any{"servers": []any{"127.0.0.1:9"}, "strategy": "fastest"}}
		}, want: `upstreams.groups.default: unknown strategy "fastest"`},
		{name: "conditional to unknown group", mutate: func(c map[string]any) {
			c["upstreams"].(map[string]any)["conditional"] = map[string]any{"corp.example.": "nope"}
		}, want: `unknown group "nope"`},
		{name: "bad DNS protocol", mutate: func(c map[string]any) {
			c["listeners"].(map[string]any)["dns"] = []any{map[string]any{"address": spec.Ports.DNS, "protocols": []any{"sctp"}}}
		}, want: `unsupported protocol "sctp"`},
		{name: "bad log level", mutate: func(c map[string]any) { c["log"] = map[string]any{"level": "loud"} }, want: `log.level: unknown level "loud"`},
		{name: "tsig key without algorithm", mutate: func(c map[string]any) {
			c["tsigKeys"] = []any{map[string]any{"name": "k", "algorithm": "hmac-md5", "secretFile": secretMissing}}
		}, want: `tsigKeys[0].algorithm`},
		{name: "tsig secret file missing", mutate: func(c map[string]any) {
			c["tsigKeys"] = []any{map[string]any{"name": "k", "algorithm": "hmac-sha256", "secretFile": secretMissing}}
		}, want: "does-not-exist.secret"},
		{name: "querylog sink", mutate: func(c map[string]any) { c["querylog"] = map[string]any{"enabled": true, "sink": "kafka"} }, want: `querylog.sink: unknown sink "kafka"`},
		{name: "invalid YAML", raw: "node: { name: [unclosed\n", want: "config"},
		{name: "unknown environment key", env: []string{"KUDURRU_NODE__BOGUS=1"}, want: "KUDURRU_NODE__BOGUS does not match a settings key"},
		{name: "bad encryption key", env: []string{"KUDURRU_ENCRYPTION_KEY=short"}, want: "KUDURRU_ENCRYPTION_KEY"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("case-%d.yaml", i))
			if c.raw != "" {
				if err := os.WriteFile(path, []byte(c.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				cfg := kudurrutest.BaseConfig("invalid", filepath.Join(dir, fmt.Sprintf("data-%d", i)), spec.Ports)
				if c.mutate != nil {
					c.mutate(cfg)
				}
				kudurrutest.WriteConfig(t, path, cfg)
			}
			code, out, d := runExpectFailure(t, c.env, "serve", "--config", path)
			if code == 0 {
				t.Fatalf("exit 0, want non-zero:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("output lacks %q:\n%s", c.want, out)
			}
			if !strings.Contains(out, "error:") {
				t.Errorf("no error: line on stderr:\n%s", out)
			}
			if d > 10*time.Second {
				t.Errorf("took %s to fail", d)
			}
		})
	}
	t.Run("missing explicit config file", func(t *testing.T) {
		code, out, _ := runExpectFailure(t, nil, "serve", "--config", filepath.Join(dir, "nope.yaml"))
		if code == 0 || !strings.Contains(out, "nope.yaml") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})
}

// TestPortConflictsFailFast occupies one listener address at a time and
// expects the node to exit non-zero within seconds, naming the address.
func TestPortConflictsFailFast(t *testing.T) {
	kudurrutest.RequireE2E(t)
	dir := t.TempDir()
	for _, which := range []string{"dns-udp", "dns-tcp", "api", "ops", "metrics", "internal"} {
		t.Run(which, func(t *testing.T) {
			spec, cfg := kudurrutest.NewNodeSpec(t, dir, which)
			kudurrutest.WriteConfig(t, spec.Config, cfg)
			var addr string
			switch which {
			case "dns-udp":
				addr = spec.Ports.DNS
				pc, err := net.ListenPacket("udp", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = pc.Close() }()
			default:
				addr = map[string]string{"dns-tcp": spec.Ports.DNS, "api": spec.Ports.API, "ops": spec.Ports.Ops, "metrics": spec.Ports.Metrics, "internal": spec.Ports.Internal}[which]
				l, err := net.Listen("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = l.Close() }()
			}
			env := []string{"KUDURRU_ENCRYPTION_KEY=" + strings.Repeat("ab", 32)}
			code, out, d := runExpectFailure(t, env, "serve", "--config", spec.Config)
			t.Logf("timing: %s conflict -> exit %d in %s", which, code, d.Round(time.Millisecond))
			if code == 0 {
				t.Fatalf("exit 0 with %s taken:\n%s", addr, out)
			}
			if !strings.Contains(out, "address already in use") || !strings.Contains(out, addr) {
				t.Errorf("output does not name the conflict on %s:\n%s", addr, out)
			}
			if d > 10*time.Second {
				t.Errorf("took %s to fail", d)
			}
		})
	}
	t.Run("data directory in use", func(t *testing.T) {
		spec, cfg := kudurrutest.NewNodeSpec(t, dir, "owner")
		kudurrutest.WriteConfig(t, spec.Config, cfg)
		kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
		other, ocfg := kudurrutest.NewNodeSpec(t, dir, "intruder")
		ocfg["node"].(map[string]any)["dataDir"] = spec.DataDir
		ocfg["auth"] = map[string]any{"adminSocket": map[string]any{"path": filepath.Join(dir, "intruder.sock")}}
		kudurrutest.WriteConfig(t, other.Config, ocfg)
		code, out, d := runExpectFailure(t, nil, "serve", "--config", other.Config)
		if code == 0 || !strings.Contains(out, "another kudurru process owns state.db") {
			t.Errorf("second process on one data directory: exit %d in %s:\n%s", code, d, out)
		}
	})
}

// TestReadyMeansServing restarts a node a few times and sends a query the
// moment /healthz/ready first answers 200: a ready node must answer its
// primary zones (§5.2: readiness means every zone watch synced).
func TestReadyMeansServing(t *testing.T) {
	kudurrutest.RequireE2E(t)
	spec, cfg := kudurrutest.NewNodeSpec(t, t.TempDir(), "ready")
	cfg["objects"] = zoneObjects(map[string]string{"ns1": "127.0.0.1", "host": "192.0.2.1"})
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	var failures []string
	for run := range 4 {
		n := kudurrutest.Start(t, kudurruBin, spec, kudurrutest.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := n.WaitReady(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		r, err := exchange(spec.Ports.DNS, "host.e2e.test.", dns.TypeA, false)
		switch {
		case err != nil:
			failures = append(failures, fmt.Sprintf("run %d: %v", run, err))
		case r.Rcode != dns.RcodeSuccess || firstA(r) != "192.0.2.1":
			failures = append(failures, fmt.Sprintf("run %d: %s", run, dns.RcodeToString[r.Rcode]))
		}
		n.Stop(20 * time.Second)
	}
	if len(failures) > 0 {
		t.Errorf("first query after readiness failed: %v", failures)
	}
}
