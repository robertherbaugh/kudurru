package edge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var testCreds = Credentials{ConsoleKey: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", APIKey: "api", Password: "pw"}

func TestBuildDefaults(t *testing.T) {
	top, err := Build(Options{ServerID: "e"}, []Backend{{Name: "n1", Address: "10.0.0.1:5353"}}, testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Binds) != 1 || top.Binds[0].Address != "0.0.0.0:53" || top.Binds[0].Protocol != Do53 || !top.Binds[0].ReusePort {
		t.Errorf("binds %+v", top.Binds)
	}
	r, _ := top.Pool(PoolResolver)
	a, _ := top.Pool(PoolAuthoritative)
	if r.Policy != "chashed" || r.Cache == nil || a.Policy != "leastOutstanding" || a.Cache != nil {
		t.Errorf("pools %+v %+v (the authoritative pool is uncached by default)", r, a)
	}
	b := top.Backends[0]
	if b.ID != BackendID("n1") || !b.ProxyProtocol || b.Weight != 100 || !slices.Equal(b.Pools, []string{PoolResolver, PoolAuthoritative}) {
		t.Errorf("backend %+v", b)
	}
	if top.Webserver.Address != "127.0.0.1:8083" || top.Console.Address != "127.0.0.1:5199" || top.Console.Key != testCreds.ConsoleKey {
		t.Errorf("console/webserver %+v %+v", top.Console, top.Webserver)
	}
	if !slices.Equal(top.ACL, DefaultACL) {
		t.Errorf("acl %v", top.ACL)
	}
	// Anonymous backends get an id from their address.
	top, _ = Build(Options{ServerID: "e"}, []Backend{{Address: "10.0.0.2:5353"}}, testCreds)
	if top.Backends[0].ID != BackendID("10.0.0.2:5353") {
		t.Error("anonymous backend id")
	}
}

func TestBuildTLSAndCache(t *testing.T) {
	top, err := Build(Options{ServerID: "e", CertFile: "/c", KeyFile: "/k", Listen: []string{"0.0.0.0", "::"},
		Ports: map[Protocol]int{DoH: 8443}, AuthoritativeCache: true}, nil, testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Binds) != 10 {
		t.Fatalf("%d binds, want 5 protocols x 2 addresses", len(top.Binds))
	}
	var doh []string
	for _, b := range top.Binds {
		if b.Protocol == DoH {
			doh = append(doh, b.Address)
			if !slices.Equal(b.DoHPaths, []string{"/dns-query"}) || b.CertFile != "/c" {
				t.Errorf("doh bind %+v", b)
			}
		}
	}
	if !slices.Equal(doh, []string{"0.0.0.0:8443", "[::]:8443"}) {
		t.Errorf("doh binds %v", doh)
	}
	if a, _ := top.Pool(PoolAuthoritative); a.Cache == nil || a.Cache.MaxTTL != 30 || a.Cache.Name != "authoritative-cache" {
		t.Errorf("authoritative cache %+v", a.Cache)
	}
	if _, err := Build(Options{Protocols: []Protocol{DoT}}, nil, testCreds); err == nil {
		t.Error("DoT without a certificate")
	}
}

func TestNormalizeZones(t *testing.T) {
	got := NormalizeZones([]string{"Example.COM", "example.com.", ".", " lan ", ""})
	if !slices.Equal(got, []string{"example.com.", "lan."}) {
		t.Errorf("%v", got)
	}
}

func TestValidSecret(t *testing.T) {
	if err := ValidSecret([]byte(strings.Repeat("a", 32))); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{strings.Repeat("a", 31), strings.Repeat("a", 31) + " ", strings.Repeat("a", 31) + `"`, strings.Repeat("\xff", 40)} {
		if ValidSecret([]byte(bad)) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseProtocol(t *testing.T) {
	for _, s := range []string{"do53", "DoT", "DOH", "doq", "doh3"} {
		if _, err := ParseProtocol(s); err != nil {
			t.Error(err)
		}
	}
	if _, err := ParseProtocol("dnscrypt"); err == nil {
		t.Error("dnscrypt accepted")
	}
}

func TestFindTLS(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := FindTLS(dir); !errors.Is(err, ErrNoTLS) {
		t.Fatalf("empty dir: %v", err)
	}
	for _, f := range []string{"fullchain.pem", "privkey.pem"} {
		_ = os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
	}
	c, k, err := FindTLS(dir)
	if err != nil || filepath.Base(c) != "fullchain.pem" || filepath.Base(k) != "privkey.pem" {
		t.Errorf("%s %s %v", c, k, err)
	}
	for _, f := range []string{"tls.crt", "tls.key"} {
		_ = os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
	}
	if c, _, _ := FindTLS(dir); filepath.Base(c) != "tls.crt" {
		t.Errorf("tls.crt must win, got %s", c)
	}
}

func TestNewCredentials(t *testing.T) {
	a, err := NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewCredentials()
	if a == b || len(a.APIKey) != 64 || len(a.ConsoleKey) != 44 {
		t.Errorf("%+v %+v", a, b)
	}
	top := &Topology{}
	a.Apply(top)
	if top.Webserver.APIKey != a.APIKey || top.Console.Key != a.ConsoleKey || top.Webserver.Password != a.Password {
		t.Error("Apply")
	}
}

func TestFileHelpers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.conf")
	if err := WriteFileAtomic(p, []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	cand := filepath.Join(dir, "f.candidate.conf")
	_ = WriteFileAtomic(cand, []byte("two"), 0o600)
	if err := SwapFile(cand, p); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(p)            //nolint:gosec // test-owned path
	prev, _ := os.ReadFile(p + ".prev") //nolint:gosec // test-owned path
	if string(cur) != "two" || string(prev) != "one" {
		t.Errorf("cur %q prev %q", cur, prev)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("leftover temp files: %v", entries)
	}
}

func TestChildAndCheck(t *testing.T) {
	if _, err := Check(context.Background(), "/bin/sh", "-c", "echo bad config; exit 3"); err == nil || !strings.Contains(err.Error(), "bad config") {
		t.Errorf("Check failure: %v", err)
	}
	if out, err := Check(context.Background(), "/bin/sh", "-c", "echo ok"); err != nil || out != "ok" {
		t.Errorf("Check: %q %v", out, err)
	}
	c := &Child{Path: "/bin/sh", Args: []string{"-c", "trap '' TERM; sleep 30"}, StopTimeout: 200 * time.Millisecond}
	if c.Running() {
		t.Fatal("running before start")
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if !c.Running() || c.Pid() == 0 {
		t.Fatal("not running")
	}
	if err := c.Start(); err == nil {
		t.Error("double start")
	}
	time.Sleep(50 * time.Millisecond) // let the shell install the trap
	start := time.Now()
	c.Stop() // SIGTERM is ignored: SIGKILL after StopTimeout
	if c.Running() || time.Since(start) < 150*time.Millisecond {
		t.Errorf("Stop: running=%v after %v", c.Running(), time.Since(start))
	}
	if c.Err() == nil {
		t.Error("killed child reported success")
	}
	_ = c.Signal(os.Interrupt) // no-op after exit
	if err := (&Child{Path: "/nonexistent/dnsdist"}).Start(); err == nil {
		t.Error("missing binary started")
	}
}
