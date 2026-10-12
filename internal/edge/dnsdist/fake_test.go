package dnsdist

// A fake dnsdist for the supervisor tests: the test binary re-executes
// itself through a symlink named "dnsdist". It validates the rendered YAML
// syntactically (--check-config), serves a minimal
// /api/v1/servers/localhost and DELETE /api/v1/cache on the rendered
// webserver address, logs what it did to <config dir>/fake.log and runs
// until SIGTERM. The real validator runs in `make edge-check`.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "dnsdist" {
		os.Exit(fakeDNSDist(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeConfig struct {
	apiAddr  string
	apiKey   string
	backends []string
}

var (
	reWebserver = regexp.MustCompile(`webserver\("([^"]+)"\)`)
	reAPIKey    = regexp.MustCompile(`apiKey="([^"]+)"`)
	reServer    = regexp.MustCompile(`name="([^"]+)", pool=`)
)

func parseFake(path string) (*fakeConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // test fake
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(data), "FAIL-CHECK") {
		return nil, fmt.Errorf("fake dnsdist: refusing %s (contains FAIL-CHECK)", path)
	}
	fc := &fakeConfig{}
	if strings.HasSuffix(path, ".yml") {
		var doc struct {
			Webserver struct {
				ListenAddresses []string `yaml:"listen_addresses"`
				APIKey          string   `yaml:"api_key"`
			} `yaml:"webserver"`
			Console  map[string]any   `yaml:"console"`
			Binds    []map[string]any `yaml:"binds"`
			Backends []struct {
				Name string `yaml:"name"`
			} `yaml:"backends"`
			QueryRules []map[string]any `yaml:"query_rules"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("fake dnsdist: %s: %w", path, err)
		}
		if len(doc.Webserver.ListenAddresses) != 1 || doc.Console == nil || len(doc.Binds) == 0 || len(doc.QueryRules) == 0 {
			return nil, fmt.Errorf("fake dnsdist: %s: missing webserver, console, binds or query_rules", path)
		}
		if _, err := os.Stat(strings.TrimSuffix(path, ".yml") + ".lua"); err != nil { //nolint:gosec // test fake
			return nil, fmt.Errorf("fake dnsdist: sibling lua missing: %w", err)
		}
		fc.apiAddr, fc.apiKey = doc.Webserver.ListenAddresses[0], doc.Webserver.APIKey
		for _, b := range doc.Backends {
			fc.backends = append(fc.backends, b.Name)
		}
		return fc, nil
	}
	m1, m2 := reWebserver.FindSubmatch(data), reAPIKey.FindSubmatch(data)
	if m1 == nil || m2 == nil {
		return nil, fmt.Errorf("fake dnsdist: %s: no webserver", path)
	}
	fc.apiAddr, fc.apiKey = string(m1[1]), string(m2[1])
	for _, m := range reServer.FindAllSubmatch(data, -1) {
		fc.backends = append(fc.backends, string(m[1]))
	}
	return fc, nil
}

func fakeLog(dir, format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(dir, "fake.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test-owned path
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, format+"\n", args...)
	_ = f.Close()
}

func fakeDNSDist(args []string) int {
	var cfgPath string
	check, supervised := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--check-config":
			check = true
		case "--supervised":
			supervised = true
		case "-C":
			i++
			if i < len(args) {
				cfgPath = args[i]
			}
		}
	}
	fc, err := parseFake(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dir := filepath.Dir(cfgPath)
	if check {
		fakeLog(dir, "check %s", filepath.Base(cfgPath))
		fmt.Printf("Configuration '%s' OK!\n", cfgPath)
		return 0
	}
	if !supervised {
		fmt.Fprintln(os.Stderr, "fake dnsdist: expected --supervised")
		return 2
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/servers/localhost", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != fc.apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		state := "up"
		if _, err := os.Stat(filepath.Join(dir, "fake-down")); err == nil { //nolint:gosec // test fake
			state = "down"
		}
		servers := []map[string]any{}
		for i, b := range fc.backends {
			servers = append(servers, map[string]any{"id": fmt.Sprint(i), "name": b, "address": b, "state": state})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"daemon_type": "dnsdist", "version": "fake", "servers": servers})
	})
	mux.HandleFunc("DELETE /api/v1/cache", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != fc.apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		fakeLog(dir, "flush pool=%s name=%s suffix=%s", q.Get("pool"), q.Get("name"), q.Get("suffix"))
		_, _ = w.Write([]byte(`{"count":"1","status":"purged"}`))
	})
	srv := &http.Server{Addr: fc.apiAddr, Handler: mux} //nolint:gosec // test fake
	go func() { _ = srv.ListenAndServe() }()
	fakeLog(dir, "start pid=%d backends=%s", os.Getpid(), strings.Join(fc.backends, ","))
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	for s := range sig {
		if s == syscall.SIGHUP {
			fakeLog(dir, "hup")
			continue
		}
		break
	}
	_ = srv.Shutdown(context.Background())
	fakeLog(dir, "stop pid=%d", os.Getpid())
	return 0
}
