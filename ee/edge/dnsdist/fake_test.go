package dnsdist

// A fake dnsdist for the supervisor tests: the test binary re-executes
// itself through a symlink named "dnsdist". It validates the rendered YAML
// syntactically (--check-config), serves a minimal
// /api/v1/servers/localhost and DELETE /api/v1/cache on the rendered
// webserver address, logs what it did to <config dir>/fake.log and runs
// until SIGTERM. The real validator runs in `make edge-check`.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge"
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
	var cfgPath, command string
	check, supervised := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-e":
			i++
			if i < len(args) {
				command = args[i]
			}
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
	if command != "" && os.Getenv("KUDURRU_FAKE_CONSOLE_MODE") != "" {
		marker := regexp.MustCompile(`KUDURRU_ACK_[a-f0-9]+`).FindString(command)
		switch os.Getenv("KUDURRU_FAKE_CONSOLE_MODE") {
		case "sleep":
			time.Sleep(time.Minute)
		case "overflow":
			fmt.Print(strings.Repeat("x", maxConsoleOutput+1))
			fmt.Print(marker)
		case "stderr-overflow":
			fmt.Fprint(os.Stderr, strings.Repeat("x", maxConsoleOutput+1))
			fmt.Print(marker)
		case "preamble":
			fmt.Print("local configuration-only diagnostic\n" + marker)
		case "trailing":
			fmt.Print(marker + "\ncommand failed")
		case "echo":
			fmt.Print(command)
		case "unacknowledged":
			fmt.Print("sensitive test material")
		case "failed":
			fmt.Print(marker)
			return 1
		default:
			fmt.Print(marker)
		}
		return 0
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
	if command != "" {
		req, _ := http.NewRequest("POST", "http://"+fc.apiAddr+"/test-console", strings.NewReader(command))
		req.Header.Set("X-API-Key", fc.apiKey)
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // test fake API address is allocated on loopback by the harness
		if err != nil {
			return 1
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(os.Stdout, resp.Body)
		if resp.StatusCode != 200 {
			return 1
		}
		return 0
	}
	if !supervised {
		fmt.Fprintln(os.Stderr, "fake dnsdist: expected --supervised")
		return 2
	}
	var stateMu sync.Mutex
	backends := map[string]string{}
	for _, name := range fc.backends {
		backends[edge.BackendID(name)] = name
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /test-console", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != fc.apiKey {
			w.WriteHeader(401)
			return
		}
		data, _ := io.ReadAll(io.LimitReader(r.Body, maxConsoleCommand+1))
		script := string(data)
		stateMu.Lock()
		defer stateMu.Unlock()
		verb := "ping"
		switch {
		case strings.Contains(script, ":isUp()"):
			verb = "health"
		case strings.Contains(script, ":setAuto("):
			verb = "activate"
		case strings.Contains(script, "newServer("):
			verb = "stage"
			id := regexp.MustCompile(`id="([^"]+)"`).FindStringSubmatch(script)
			name := regexp.MustCompile(`name="([^"]+)"`).FindStringSubmatch(script)
			if len(id) == 2 && len(name) == 2 {
				backends[id[1]] = name[1]
			}
		case strings.Contains(script, ":addPool("):
			verb = "promote"
		case strings.Contains(script, ":setDown("):
			verb = "down"
		case strings.Contains(script, "rmServer("):
			verb = "remove"
			id := regexp.MustCompile(`rmServer\("([^"]+)"\)`).FindStringSubmatch(script)
			if len(id) == 2 {
				delete(backends, id[1])
			}
		case strings.Contains(script, "reloadAllCertificates()"):
			verb = "reload"
		}
		fakeLog(dir, "console %s", verb)
		// Lost acknowledgment deliberately follows mutation, exercising recovery.
		if _, err := os.Stat(filepath.Join(dir, "fail-"+verb)); err == nil { //nolint:gosec // bounded verb and owned fake fixture directory
			return
		}
		if verb == "health" {
			if _, err := os.Stat(filepath.Join(dir, "reject-health")); err == nil { //nolint:gosec // owned fake fixture fault file
				return
			}
		}
		marker := regexp.MustCompile(`KUDURRU_ACK_[a-f0-9]+`).Find(data)
		_, _ = w.Write(bytes.Clone(marker))
	})
	mux.HandleFunc("/api/v1/servers/localhost", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != fc.apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		state := "up"
		if _, err := os.Stat(filepath.Join(dir, "fake-down")); err == nil { //nolint:gosec // test fake
			state = "down"
		}
		stateMu.Lock()
		servers := []map[string]any{}
		for _, b := range backends {
			servers = append(servers, map[string]any{"id": len(servers), "name": b, "address": b, "state": state})
		}
		stateMu.Unlock()
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
