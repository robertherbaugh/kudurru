package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type mapKeyring struct {
	m    map[string]string
	fail bool
}

func (k *mapKeyring) Get(s, u string) (string, error) {
	if k.fail {
		return "", errors.New("locked")
	}
	v, ok := k.m[s+"/"+u]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (k *mapKeyring) Set(s, u, v string) error {
	if k.fail {
		return errors.New("locked")
	}
	k.m[s+"/"+u] = v
	return nil
}

func (k *mapKeyring) Delete(s, u string) error {
	delete(k.m, s+"/"+u)
	return nil
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultConfigPath(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{EnvCLIConfig: "/x/c.yaml", "HOME": "/home/u"}, "/x/c.yaml"},
		{map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": "/home/u"}, "/xdg/kudurru/config.yaml"},
		{map[string]string{"HOME": "/home/u"}, "/home/u/.config/kudurru/config.yaml"},
	}
	for _, tc := range cases {
		got, err := DefaultConfigPath(env(tc.env))
		if err != nil || got != tc.want {
			t.Errorf("%v → %s, %v; want %s", tc.env, got, err, tc.want)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	cfg, err := LoadConfig(path)
	if err != nil || len(cfg.Contexts) != 0 {
		t.Fatalf("missing file: %v %+v", err, cfg)
	}
	cfg.Upsert(Context{Name: "b", Server: "https://b.example"})
	cfg.Upsert(Context{Name: "a", Server: "https://a.example", Token: "kdr_x", TokenStore: TokenStoreFile})
	cfg.Upsert(Context{Name: "b", Server: "https://b2.example"})
	if err := cfg.UseContext("b"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseContext("zzz"); err == nil || !strings.Contains(err.Error(), "have: a, b") {
		t.Fatalf("UseContext(zzz) = %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	dst, _ := os.Stat(filepath.Dir(path))
	if st.Mode().Perm() != 0o600 || dst.Mode().Perm() != 0o700 {
		t.Fatalf("modes file %v dir %v", st.Mode().Perm(), dst.Mode().Perm())
	}
	back, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.CurrentContext != "b" || len(back.Contexts) != 2 || back.Contexts[0].Name != "a" || back.Contexts[1].Server != "https://b2.example" {
		t.Fatalf("round trip %+v", back)
	}
	if !back.Remove("b") || back.CurrentContext != "" || back.Remove("b") {
		t.Fatal("Remove")
	}
	raw, _ := os.ReadFile(path) //nolint:gosec // G304: the test's temp file
	if !strings.HasPrefix(string(raw), "apiVersion: kudurru.io/v1alpha1\n") {
		t.Fatalf("file:\n%s", raw)
	}

	for name, body := range map[string]string{
		"dup":     "contexts:\n- {name: a, server: x}\n- {name: a, server: y}\n",
		"noname":  "contexts:\n- {server: x}\n",
		"unknown": "contexts: []\nbogus: 1\n",
	} {
		p := filepath.Join(t.TempDir(), name+".yaml")
		_ = os.WriteFile(p, []byte(body), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestResolvePrecedence(t *testing.T) {
	kr := &mapKeyring{m: map[string]string{KeyringService + "/home": "kdr_home"}}
	creds := &Credentials{Keyring: kr}
	cfg := &Config{CurrentContext: "home", path: "/c.yaml", Contexts: []Context{
		{Name: "home", Server: "https://home.example", TokenStore: TokenStoreKeyring, Namespace: "dns", CAFile: "/ca.pem"},
		{Name: "lab", Server: "https://lab.example", Token: "kdr_lab", TokenStore: TokenStoreFile, InsecureSkipVerify: true},
		{Name: "gone", Server: "https://gone.example", TokenStore: TokenStoreKeyring},
	}}
	cases := []struct {
		name string
		o    Overrides
		env  map[string]string
		want Resolved
		err  string
	}{
		{"current context", Overrides{}, nil, Resolved{ContextName: "home", Server: "https://home.example", Token: "kdr_home", Namespace: "dns", CAFile: "/ca.pem", TokenSource: "keyring"}, ""},
		{"--context", Overrides{Context: "lab"}, nil, Resolved{ContextName: "lab", Server: "https://lab.example", Token: "kdr_lab", InsecureSkipVerify: true, TokenSource: "file"}, ""},
		{"KUDURRU_CONTEXT", Overrides{}, map[string]string{EnvContext: "lab"}, Resolved{ContextName: "lab", Server: "https://lab.example", Token: "kdr_lab", InsecureSkipVerify: true, TokenSource: "file"}, ""},
		{"flag beats env context", Overrides{Context: "home"}, map[string]string{EnvContext: "lab"}, Resolved{ContextName: "home", Server: "https://home.example", Token: "kdr_home", Namespace: "dns", CAFile: "/ca.pem", TokenSource: "keyring"}, ""},
		{"env server elsewhere drops the context token", Overrides{}, map[string]string{EnvServer: "https://other.example"}, Resolved{Server: "https://other.example", Namespace: "dns"}, ""},
		{"env server and token", Overrides{}, map[string]string{EnvServer: "https://other.example", EnvToken: "kdr_env"}, Resolved{Server: "https://other.example", Token: "kdr_env", Namespace: "dns", TokenSource: "env"}, ""},
		{"flags beat env", Overrides{Server: "https://flag.example", Token: "kdr_flag", Namespace: "x"}, map[string]string{EnvServer: "https://other.example", EnvToken: "kdr_env"}, Resolved{Server: "https://flag.example", Token: "kdr_flag", Namespace: "x", TokenSource: "flag"}, ""},
		{"same server keeps the token", Overrides{Server: "HTTPS://home.example/"}, nil, Resolved{ContextName: "home", Server: "HTTPS://home.example/", Token: "kdr_home", Namespace: "dns", CAFile: "/ca.pem", TokenSource: "keyring"}, ""},
		{"unknown --context", Overrides{Context: "nope"}, nil, Resolved{}, `no context "nope"`},
		{"keyring entry missing", Overrides{Context: "gone"}, nil, Resolved{}, "token missing from the OS keyring"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(cfg, creds, tc.o, env(tc.env))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v\nwant %+v", got, err, tc.want)
			}
		})
	}
	if _, err := Resolve(&Config{}, creds, Overrides{}, env(nil)); err == nil || !strings.Contains(err.Error(), "kudurru login") {
		t.Fatalf("no server: %v", err)
	}
}

func TestCredentialsFallback(t *testing.T) {
	kr := &mapKeyring{m: map[string]string{}}
	creds := &Credentials{Keyring: kr}
	ctx := Context{Name: "home"}
	if store, warn := creds.Store(&ctx, "kdr_1"); store != TokenStoreKeyring || warn != nil || ctx.Token != "" {
		t.Fatalf("keyring store: %s %v %+v", store, warn, ctx)
	}
	if tok, src, err := creds.Token(ctx); tok != "kdr_1" || src != TokenStoreKeyring || err != nil {
		t.Fatalf("Token: %s %s %v", tok, src, err)
	}
	kr.fail = true
	if _, _, err := creds.Token(ctx); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("locked keyring: %v", err)
	}
	store, warn := creds.Store(&ctx, "kdr_2")
	if store != TokenStoreFile || warn == nil || ctx.Token != "kdr_2" || ctx.TokenStore != TokenStoreFile {
		t.Fatalf("fallback: %s %v %+v", store, warn, ctx)
	}
	if tok, src, _ := creds.Token(ctx); tok != "kdr_2" || src != TokenStoreFile {
		t.Fatalf("file token: %s %s", tok, src)
	}
	kr.fail = false
	creds.Forget(&ctx)
	if ctx.Token != "" || ctx.TokenStore != "" {
		t.Fatalf("Forget: %+v", ctx)
	}
	if tok, _, err := creds.Token(ctx); tok != "" || err != nil {
		t.Fatalf("no token: %q %v", tok, err)
	}
	if c := NewCredentials(env(map[string]string{EnvKeyring: "file"})); c.Keyring.Set("a", "b", "c") == nil {
		t.Fatal("KUDURRU_KEYRING=file must not use the keyring")
	}
}

func TestSystemKeyringWithMock(t *testing.T) {
	keyring.MockInit()
	var kr SystemKeyring
	if err := kr.Set(KeyringService, "ctx", "kdr_sys"); err != nil {
		t.Fatal(err)
	}
	if v, err := kr.Get(KeyringService, "ctx"); err != nil || v != "kdr_sys" {
		t.Fatalf("Get %q %v", v, err)
	}
	if err := kr.Delete(KeyringService, "ctx"); err != nil {
		t.Fatal(err)
	}
	if _, err := kr.Get(KeyringService, "ctx"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	keyring.MockInitWithError(errors.New("no dbus"))
	if err := kr.Set(KeyringService, "ctx", "x"); err == nil {
		t.Fatal("mock error not surfaced")
	}
}

func TestLookupKind(t *testing.T) {
	cases := map[string]string{
		"zone": "dnszones", "zones": "dnszones", "dnszone": "dnszones", "DNSZone": "dnszones", "dnszones.kudurru.io": "dnszones",
		"rrset": "dnsrecordsets", "DNSRecordSet": "dnsrecordsets",
		"list": "blocklists", "blocklist": "blocklists", "BlockList": "blocklists",
		"cg": "clientgroups", "ClientGroup": "clientgroups",
		"pol": "policies", "policy": "policies", "Policy": "policies",
	}
	for in, want := range cases {
		k, err := LookupKind(in)
		if err != nil || k.Resource != want {
			t.Errorf("LookupKind(%q) = %s, %v; want %s", in, k.Resource, err, want)
		}
	}
	if _, err := LookupKind("records"); err == nil || !strings.Contains(err.Error(), "dnszones, dnsrecordsets") {
		t.Errorf("unknown kind err = %v", err)
	}
	if k, _ := LookupKind("zone"); k.Namespaced {
		t.Error("DNSZone must be cluster-scoped")
	}
}

func TestParseManifests(t *testing.T) {
	src := `# leading comment only
---
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: a}
spec: {zoneName: a., type: primary}
---
# comment-only document
---
kind: PolicyList
items:
- {kind: Policy, metadata: {name: p1, namespace: ns1}}
- {kind: Policy, metadata: {name: p2}}
`
	ms, err := ParseManifests("f.yaml", strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 || ms[0].Ref() != "dnszone/a" || ms[1].Ref() != "policy/ns1/p1" || ms[2].Index != 2 {
		t.Fatalf("manifests %+v", ms)
	}
	var obj map[string]any
	if err := json.Unmarshal(ms[0].Object, &obj); err != nil || obj["spec"].(map[string]any)["zoneName"] != "a." {
		t.Fatalf("object JSON %s", ms[0].Object)
	}
	ms, err = ParseManifests("a.json", strings.NewReader(` [{"kind":"Policy","metadata":{"name":"x"}}, {"kind":"Policy","metadata":{"name":"y"}}]`))
	if err != nil || len(ms) != 2 {
		t.Fatalf("JSON array: %v %d", err, len(ms))
	}
	ms, err = ParseManifests("o.json", strings.NewReader(`{"kind":"Policy","metadata":{"name":"x"}}`))
	if err != nil || len(ms) != 1 {
		t.Fatalf("JSON object: %v %d", err, len(ms))
	}
	for _, bad := range []string{"kind: [", "metadata: {name: x}", "kind: Policy", "[1, 2"} {
		if _, err := ParseManifests("bad", strings.NewReader(bad)); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	if _, err := ReadManifests(nil, nil, false); err == nil {
		t.Error("no paths must fail")
	}
}

func TestTableAndFormats(t *testing.T) {
	tb := &Table{Headers: []string{"NAME", "VALUE"}}
	tb.Add("a", "tab\there")
	tb.Add("longer-name", "x")
	var sb strings.Builder
	if err := tb.Write(&sb); err != nil {
		t.Fatal(err)
	}
	want := "NAME          VALUE\na             tab here\nlonger-name   x\n"
	if sb.String() != want {
		t.Fatalf("table:\n%q\nwant\n%q", sb.String(), want)
	}
	for in, want := range map[string]Format{"": FormatTable, "WIDE": FormatWide, "json": FormatJSON, "yml": FormatYAML, "name": FormatName} {
		if f, err := ParseFormat(in); err != nil || f != want {
			t.Errorf("ParseFormat(%q) = %s %v", in, f, err)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("xml accepted")
	}
	sb.Reset()
	_ = WriteStructured(&sb, FormatYAML, json.RawMessage(`{"b":1,"a":[true]}`))
	if sb.String() != "a:\n- true\nb: 1\n" {
		t.Fatalf("yaml %q", sb.String())
	}
}

func TestAgeAndReady(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if got := Age(metav1Time(now.Add(-90*time.Second)), now); got != "90s" {
		t.Errorf("age %s", got)
	}
	if got := Age(metav1Time(time.Time{}), now); got != "<unknown>" {
		t.Errorf("zero age %s", got)
	}
	if ReadyStatus(nil) != "Unknown" {
		t.Error("ready of nothing")
	}
}

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kdr-cli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestAdminClientNDJSON(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "a.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			var req AdminRequest
			if json.Unmarshal([]byte(line), &req) != nil {
				_, _ = c.Write([]byte(`{"ok":false,"error":"bad request"}` + "\n"))
				_ = c.Close()
				continue
			}
			switch req.Op {
			case "echo":
				b, _ := json.Marshal(map[string]any{"ok": true, "result": req.Args})
				_, _ = c.Write(append(b, '\n'))
			case "string-error":
				_, _ = c.Write([]byte(`{"ok":false,"error":"denied: uid 1000"}` + "\n"))
			case "garbage":
				_, _ = c.Write([]byte("not json\n"))
			default:
				_, _ = c.Write([]byte(`{"ok":false,"error":{"key":"unknown_op","message":"what"}}` + "\n"))
			}
			_ = c.Close()
		}
	}()
	ctx := context.Background()
	for _, proto := range []string{AdminProtocolNDJSON, AdminProtocolAuto} {
		a := &AdminClient{Socket: sock, Protocol: proto, Timeout: 5 * time.Second}
		var out map[string]string
		if err := a.Call(ctx, "echo", map[string]string{"a": "b"}, &out); err != nil || out["a"] != "b" {
			t.Fatalf("%s echo: %v %v", proto, out, err)
		}
		if err := a.Call(ctx, "string-error", nil, nil); err == nil || err.Error() != "admin string-error: denied: uid 1000" {
			t.Fatalf("%s string error: %v", proto, err)
		}
		var ae *AdminError
		if err := a.Call(ctx, "nope", nil, nil); !errors.As(err, &ae) || ae.Key != "unknown_op" {
			t.Fatalf("%s object error: %v", proto, err)
		}
	}
	a := &AdminClient{Socket: sock, Protocol: AdminProtocolNDJSON}
	if err := a.Call(ctx, "garbage", nil, nil); err == nil || !strings.Contains(err.Error(), "bad response") {
		t.Fatalf("garbage: %v", err)
	}
	if err := (&AdminClient{Socket: sock, Protocol: "smoke"}).Call(ctx, "echo", nil, nil); err == nil {
		t.Fatal("unknown protocol accepted")
	}
	missing := &AdminClient{Socket: filepath.Join(filepath.Dir(sock), "none.sock")}
	if err := missing.Call(ctx, "echo", nil, nil); err == nil || !strings.Contains(err.Error(), "inside the kudurru container") {
		t.Fatalf("missing socket: %v", err)
	}
}

func TestAdminClientHTTP(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/v1/create-token", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/json" || string(b) != `{"user":"bob"}` {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"key":"invalid","message":"unexpected body"}}`)
			return
		}
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":"t1","token":"kdr_t1_x"}`)
	})
	mux.HandleFunc("GET /admin/v1/list-users", func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > 0 {
			w.WriteHeader(400)
			return
		}
		_, _ = io.WriteString(w, `{"users":[]}`)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()

	a := &AdminClient{Socket: sock}
	ctx := context.Background()
	var tok struct{ ID, Token string }
	if err := a.Call(ctx, AdminCreateToken, map[string]string{"user": "bob"}, &tok); err != nil || tok.Token != "kdr_t1_x" {
		t.Fatalf("create-token: %+v %v", tok, err)
	}
	var users map[string][]any
	if err := a.Call(ctx, AdminListUsers, nil, &users); err != nil || users["users"] == nil {
		t.Fatalf("list-users: %v %v", users, err)
	}
	var ae *AdminError
	if err := a.Call(ctx, AdminCreateToken, map[string]string{"user": "eve"}, nil); !errors.As(err, &ae) || ae.Key != "invalid" {
		t.Fatalf("API error: %v", err)
	}
	// An unknown route is a plain 404, still an AdminError.
	if err := (&AdminClient{Socket: sock, Protocol: AdminProtocolHTTP}).Call(ctx, "nope", nil, nil); !errors.As(err, &ae) {
		t.Fatalf("404: %v", err)
	}
}

func metav1Time(t time.Time) metav1.Time {
	if t.IsZero() {
		return metav1.Time{}
	}
	return metav1.NewTime(t)
}
