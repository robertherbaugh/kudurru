package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// tsigCLICommands stands in for the durable command and its source behind the
// production router only; routing, permission checks, strict body decoding,
// receipt validation and status selection are the real server's.
type tsigCLICommands struct {
	mu      sync.Mutex
	changes []api.TSIGKeyChange
	next    func(api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error)
	keys    []api.TSIGKeyMetadata
}

func (s *tsigCLICommands) command(_ context.Context, change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change.Secret = bytes.Clone(change.Secret)
	s.changes = append(s.changes, change)
	return s.next(change)
}

func (s *tsigCLICommands) last(t *testing.T) api.TSIGKeyChange {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.changes) == 0 {
		t.Fatal("no stored TSIG key command reached the server")
	}
	return s.changes[len(s.changes)-1]
}

func (s *tsigCLICommands) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.changes)
}

func tsigCLIReceipt(change api.TSIGKeyChange, published bool, rv string) *api.TSIGKeyCommandReceipt {
	created := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	uid := change.UID
	if change.Operation == api.TSIGKeyCreate {
		uid = "0f9c2b1e-1111-4222-8333-944445555666"
	}
	if change.Operation == api.TSIGKeyRevoke {
		rv = change.ResourceVersion
	}
	algorithm := change.Algorithm
	if algorithm == "" {
		algorithm = "hmac-sha256."
	}
	return &api.TSIGKeyCommandReceipt{Operation: change.Operation, Committed: true, Published: published, Removed: change.Operation == api.TSIGKeyRevoke,
		Key: api.TSIGKeyMetadata{KeyName: change.KeyName, Algorithm: algorithm, Source: "store", UID: uid, ResourceVersion: rv, CreatedAt: &created}}
}

func newTSIGRouterCLI(t *testing.T) (*routerCLI, *tsigCLICommands) {
	t.Helper()
	commands := &tsigCLICommands{}
	f := newRouterCLI(t, func(o *api.Options) {
		o.TSIGKeyCommand = commands.command
		o.TSIGKeyReads = func(context.Context) ([]api.TSIGKeyMetadata, error) {
			commands.mu.Lock()
			defer commands.mu.Unlock()
			return append([]api.TSIGKeyMetadata(nil), commands.keys...), nil
		}
	})
	return f, commands
}

// The CLI sends each stored-key command once through the production router,
// reads the secret only from a file or stdin, renders the 201/200/202 receipt
// and never echoes the secret, and keeps the server's refusals unretried.
func TestCLIRealRouterTSIGKeys(t *testing.T) {
	f, commands := newTSIGRouterCLI(t)
	secret := bytes.Repeat([]byte{0x5a, 0xc3, 0x11}, 22) // 66 bytes: wraps at 64 base64 columns
	encoded := base64.StdEncoding.EncodeToString(secret)
	wrapped := encoded[:64] + "\n" + encoded[64:] + "\n"
	secretFile := filepath.Join(t.TempDir(), "transfer.secret")
	if err := os.WriteFile(secretFile, []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	noSecret := func(t *testing.T, outputs ...string) {
		t.Helper()
		for _, s := range outputs {
			if strings.Contains(s, encoded[:24]) || strings.Contains(s, encoded[64:]) {
				t.Fatal("CLI output contains TSIG secret material")
			}
		}
	}

	commands.next = func(change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
		return tsigCLIReceipt(change, true, "41"), nil
	}
	out, stderr, err := f.run("tsigkeys", "create", "transfer-key", "--algorithm", "hmac-sha256", "--secret-file", secretFile, "-o", "json")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, stderr)
	}
	noSecret(t, out, stderr)
	change := commands.last(t)
	if change.Operation != api.TSIGKeyCreate || change.KeyName != "transfer-key." || change.Algorithm != "hmac-sha256." || !bytes.Equal(change.Secret, secret) {
		t.Fatal("create did not deliver the exact name, algorithm and decoded secret")
	}
	var receipt api.TSIGKeyCommandReceipt
	if json.Unmarshal([]byte(out), &receipt) != nil || receipt.Operation != api.TSIGKeyCreate || !receipt.Committed || !receipt.Published || receipt.Key.UID == "" || receipt.Key.ResourceVersion != "41" {
		t.Fatal("create did not render the 201 receipt")
	}
	if f.count(http.MethodPost, "/api/v1/tsigkeys") != 1 {
		t.Fatal("create was not sent exactly once")
	}

	commands.mu.Lock()
	commands.keys = []api.TSIGKeyMetadata{
		{KeyName: "file-key.", Algorithm: "hmac-sha512.", Source: "file"},
		receipt.Key,
	}
	commands.mu.Unlock()
	out, _, err = f.run("tsigkeys", "list")
	if err != nil || !strings.Contains(out, "RESOURCEVERSION") || !strings.Contains(out, receipt.Key.UID) || !strings.Contains(out, "file-key.") {
		t.Fatalf("list table lost metadata: %v\n%s", err, out)
	}

	// Replace reads the secret from stdin and keeps the current algorithm. A
	// commit without publication acknowledgment is HTTP 202: a success that
	// says so, never a rollback and never a retry.
	commands.next = func(change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
		return tsigCLIReceipt(change, false, "42"), nil
	}
	replacement := bytes.Repeat([]byte{0x7e}, 32)
	f.stdin = base64.StdEncoding.EncodeToString(replacement) + "\n"
	out, stderr, err = f.run("tsigkeys", "replace", "transfer-key.", "--uid", receipt.Key.UID, "--resource-version", "41", "--secret-file", "-")
	f.stdin = ""
	if err != nil {
		t.Fatalf("replace: %v\n%s", err, stderr)
	}
	change = commands.last(t)
	if change.Operation != api.TSIGKeyReplace || change.KeyName != "transfer-key." || change.UID != receipt.Key.UID || change.ResourceVersion != "41" || change.Algorithm != "" || !bytes.Equal(change.Secret, replacement) {
		t.Fatal("replace did not deliver the exact reviewed binding and stdin secret")
	}
	if !strings.Contains(stderr, "HTTP 202") || !strings.Contains(stderr, "not a rollback") || !strings.Contains(out, "false") {
		t.Fatalf("202 receipt was not explained\nstdout:\n%s\nstderr:\n%s", out, stderr)
	}
	noSecret(t, out, stderr)
	if strings.Contains(out+stderr, base64.StdEncoding.EncodeToString(replacement)) {
		t.Fatal("replace output contains the secret")
	}

	commands.next = func(change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
		return tsigCLIReceipt(change, true, "43"), nil
	}
	out, _, err = f.run("tsigkeys", "reseal", "transfer-key.", "--uid", receipt.Key.UID, "--resource-version", "42", "-o", "json")
	change = commands.last(t)
	if err != nil || change.Operation != api.TSIGKeyReseal || change.ResourceVersion != "42" || change.Secret != nil || json.Unmarshal([]byte(out), &receipt) != nil || receipt.Key.ResourceVersion != "43" || !receipt.Published {
		t.Fatalf("reseal did not send the binding or render the 200 receipt: %v", err)
	}
	out, _, err = f.run("tsigkeys", "revoke", "transfer-key.", "--uid", receipt.Key.UID, "--resource-version", "43", "-o", "json")
	change = commands.last(t)
	if err != nil || change.Operation != api.TSIGKeyRevoke || change.ResourceVersion != "43" || json.Unmarshal([]byte(out), &receipt) != nil || !receipt.Removed || receipt.Key.ResourceVersion != "43" {
		t.Fatalf("revoke did not render the historical receipt: %v", err)
	}
	for _, op := range []string{"replace", "reseal", "revoke"} {
		if f.count(http.MethodPost, "/api/v1/tsigkeys/transfer-key./"+op) != 1 {
			t.Fatalf("%s was not sent exactly once", op)
		}
	}
	sent := commands.count()

	// Local refusals send nothing: no reviewed binding, no secret file, a
	// secret argument, or a secret that is not canonical base64.
	for _, args := range [][]string{
		{"tsigkeys", "replace", "transfer-key.", "--secret-file", secretFile},
		{"tsigkeys", "revoke", "transfer-key.", "--uid", receipt.Key.UID},
		{"tsigkeys", "create", "other-key", "--algorithm", "hmac-sha256"},
		{"tsigkeys", "create", "other-key", "--algorithm", "hmac-sha256", "--secret", encoded},
		{"tsigkeys", "create", "other-key", "--secret-file", secretFile},
	} {
		if _, _, err := f.run(args...); err == nil {
			t.Fatalf("%v was not refused locally", args)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.secret")
	if err := os.WriteFile(bad, []byte("not*base64-but-private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = f.run("tsigkeys", "create", "other-key", "--algorithm", "hmac-sha256", "--secret-file", bad)
	if err == nil || strings.Contains(err.Error()+stderr, "private") {
		t.Fatal("an invalid secret was accepted or echoed")
	}
	if commands.count() != sent || f.count(http.MethodPost, "/api/v1/tsigkeys") != 1 {
		t.Fatal("a locally refused command reached the server")
	}

	// Server refusals keep their status and key and are sent once.
	commands.next = func(api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
		return nil, &api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyConflict, Message: "stored TSIG key binding is stale"}}
	}
	_, _, err = f.run("tsigkeys", "reseal", "transfer-key.", "--uid", receipt.Key.UID, "--resource-version", "40")
	if !apiclient.IsKey(err, api.KeyConflict) || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("stale binding lost the conflict key or retry statement: %v", err)
	}
	commands.next = func(api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
		return nil, &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "current writable serving generation unavailable"}}
	}
	_, _, err = f.run("tsigkeys", "reseal", "transfer-key.", "--uid", receipt.Key.UID, "--resource-version", "44")
	if !apiclient.IsStatus(err, http.StatusServiceUnavailable) || !apiclient.IsKey(err, api.KeyUnavailable) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("unavailable writer lost its status or uncertainty statement: %v", err)
	}
	// An invalid receipt from the command is refused by the server itself.
	commands.next = func(change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
		r := tsigCLIReceipt(change, true, "45")
		r.Committed = false
		return r, nil
	}
	_, _, err = f.run("tsigkeys", "reseal", "transfer-key.", "--uid", receipt.Key.UID, "--resource-version", "45")
	if !apiclient.IsStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("unconfirmed commit was rendered as a receipt: %v", err)
	}
	if commands.count() != sent+3 || f.count(http.MethodPost, "/api/v1/tsigkeys/transfer-key./reseal") != 4 {
		t.Fatal("a refused or uncertain command was replayed")
	}

	// Authorization is the router's: a viewer reads metadata but cannot write.
	f.env["KUDURRU_TOKEN"] = apitest.BearerPrefix + "viewer"
	_, _, err = f.run("tsigkeys", "create", "viewer-key", "--algorithm", "hmac-sha256", "--secret-file", secretFile)
	if !apiclient.IsKey(err, api.KeyForbidden) || commands.count() != sent+3 {
		t.Fatalf("viewer create was not forbidden before the command: %v", err)
	}
}

// A success status outside the operation's contract, or a missing response,
// is an unknown outcome: reported as such and never retried.
func TestCLITSIGKeyUnknownOutcome(t *testing.T) {
	var calls int
	var mu sync.Mutex
	server := newTSIGStatusServer(t, func(w http.ResponseWriter) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK) // create must answer 201 or 202
			_, _ = w.Write([]byte(`{"operation":"create","committed":true,"published":true,"removed":false,"key":{"keyName":"k.","algorithm":"hmac-sha256.","source":"store","uid":"u","resourceVersion":"1"}}`))
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server cannot drop the connection")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	h := &harness{t: t, cfgPath: filepath.Join(t.TempDir(), "cli.yaml"), env: map[string]string{"KUDURRU_TOKEN": "kdr_test"}, keyring: &memKeyring{}}
	h.stdin = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	for range 2 {
		_, _, err := h.run("--server", server, "tsigkeys", "create", "k", "--algorithm", "hmac-sha256", "--secret-file", "-")
		if err == nil || !strings.Contains(err.Error(), "outcome unknown") || !strings.Contains(err.Error(), "not retried") {
			t.Fatalf("unknown outcome was not reported: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("each command must be sent exactly once, saw %d requests for 2 commands", calls)
	}
}

func newTSIGStatusServer(t *testing.T, answer func(http.ResponseWriter)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tsigkeys" {
			http.NotFound(w, r)
			return
		}
		answer(w)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// The CLI drives the mounted complete SQLite runtime end to end: an operator
// signs in with the CLI, creates a key from a secret file, replaces it from
// stdin, reseals it, has a stale binding refused and revokes it, and signed
// DNS follows each receipt. No output contains either secret.
func TestRunServeSQLiteTSIGKeysCLI(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "management"}, Spec: public.DNSZoneSpec{ZoneName: "management.test.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "management"}, Endpoints: []public.Endpoint{{DNSName: "seed.management.test.", RecordType: "A", Targets: []string{"192.0.2.97"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed desired DNS authority")
		}
	}
	closeStore()
	tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "seed.management.test.", "192.0.2.97")
	awaitControlMountedSource(t, cfg)
	cli := mountedCLI(t, cfg, "tsig")
	list := func() api.TSIGKeyList {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			out, _, err := cli.run("tsigkeys", "list", "-o", "json")
			var keys api.TSIGKeyList
			if err == nil && json.Unmarshal([]byte(out), &keys) == nil {
				return keys
			}
			if time.Now().After(deadline) {
				t.Fatalf("tsigkeys list never answered: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	first, second := bytes.Repeat([]byte{0x6a}, 32), bytes.Repeat([]byte{0x6d}, 64)
	secrets := []string{base64.StdEncoding.EncodeToString(first), base64.StdEncoding.EncodeToString(second)}
	command := func(args ...string) api.TSIGKeyCommandReceipt {
		t.Helper()
		out, stderr, err := cli.run(append(append([]string{"tsigkeys"}, args...), "-o", "json")...)
		for _, secret := range secrets {
			if strings.Contains(out+stderr, secret) {
				t.Fatal("CLI output contains TSIG secret material")
			}
		}
		var receipt api.TSIGKeyCommandReceipt
		if err != nil || json.Unmarshal([]byte(out), &receipt) != nil || !receipt.Committed || !receipt.Published {
			t.Fatalf("tsigkeys %s: %v\n%s", args[0], err, stderr)
		}
		return receipt
	}
	keyring := func(secret []byte) *dnsserver.StaticKeyring {
		t.Helper()
		keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "managed.", Algorithm: dns.HmacSHA256, Secret: secret})
		if err != nil {
			t.Fatal("prepare signed DNS client")
		}
		return keys
	}

	list()
	secretFile := filepath.Join(t.TempDir(), "managed.secret")
	if err := os.WriteFile(secretFile, []byte(secrets[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := command("create", "managed", "--algorithm", "hmac-sha256", "--secret-file", secretFile)
	if created.Key.KeyName != "managed." || created.Key.UID == "" || created.Key.ResourceVersion == "" {
		t.Fatal("create receipt lacks the stored binding")
	}
	tsigMountSignedManagementQuery(t, cfg, keyring(first), dns.RcodeSuccess)

	list()
	cli.stdin = secrets[1]
	replaced := command("replace", "managed.", "--uid", created.Key.UID, "--resource-version", created.Key.ResourceVersion, "--secret-file", "-")
	cli.stdin = ""
	if replaced.Key.UID != created.Key.UID || replaced.Key.ResourceVersion == created.Key.ResourceVersion {
		t.Fatal("replace did not keep the UID and advance the resourceVersion")
	}
	tsigMountSignedManagementQuery(t, cfg, keyring(second), dns.RcodeSuccess)

	list()
	resealed := command("reseal", "managed.", "--uid", replaced.Key.UID, "--resource-version", replaced.Key.ResourceVersion)
	if resealed.Key.UID != replaced.Key.UID || resealed.Key.ResourceVersion == replaced.Key.ResourceVersion {
		t.Fatal("reseal did not commit a new envelope")
	}
	tsigMountSignedManagementQuery(t, cfg, keyring(second), dns.RcodeSuccess)

	// The binding read before the reseal is stale now: the server refuses it
	// and the CLI reports that refusal without retrying it.
	list()
	_, _, err := cli.run("tsigkeys", "revoke", "managed.", "--uid", replaced.Key.UID, "--resource-version", replaced.Key.ResourceVersion)
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode >= http.StatusInternalServerError || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("stale revoke was not refused as such: %v", err)
	}
	if current := tsigMountMetadata(t, list(), "managed."); current.ResourceVersion != resealed.Key.ResourceVersion {
		t.Fatal("refused stale revoke changed the stored key")
	}

	revoked := command("revoke", "managed.", "--uid", resealed.Key.UID, "--resource-version", resealed.Key.ResourceVersion)
	if !revoked.Removed || revoked.Key.ResourceVersion != resealed.Key.ResourceVersion {
		t.Fatal("revoke receipt lacks the historical binding")
	}
	tsigMountSignedManagementQuery(t, cfg, keyring(second), dns.RcodeNotAuth)
	awaitMountedDNS(t, cfg, "seed.management.test.", "192.0.2.97")
}

// On an enrolled follower the production router refuses /tsigkeys commands
// with 503 primary_required before the handler: the CLI says nothing changed,
// keeps the key for the primary hint, and sends the command once.
func TestCLIRealRouterTSIGKeysFollowerRefusal(t *testing.T) {
	commands := &tsigCLICommands{next: func(change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
		return tsigCLIReceipt(change, true, "1"), nil
	}}
	f := newRouterCLI(t, func(o *api.Options) {
		o.TSIGKeyCommand = commands.command
		o.TSIGKeyReads = func(context.Context) ([]api.TSIGKeyMetadata, error) { return nil, nil }
		o.ClusterRole = func(context.Context) api.ClusterRole { return api.ClusterRole{Follower: true, Primary: "node-a"} }
	})
	if _, _, err := f.run("tsigkeys", "list"); err != nil {
		t.Fatalf("a follower must still list metadata: %v", err)
	}
	_, _, err := f.run("tsigkeys", "reseal", "managed.", "--uid", "0f9c2b1e-1111-4222-8333-944445555666", "--resource-version", "7")
	if !apiclient.IsKey(err, api.KeyPrimaryRequired) || !apiclient.IsStatus(err, http.StatusServiceUnavailable) ||
		!strings.Contains(err.Error(), "nothing changed") || strings.Contains(err.Error(), "uncertain") || !strings.Contains(errorHint(err), "run it against the primary") {
		t.Fatalf("follower refusal did not read as a known refusal: %v", err)
	}
	if commands.count() != 0 || f.count(http.MethodPost, "/api/v1/tsigkeys/managed./reseal") != 1 {
		t.Fatal("follower refusal reached the command or was replayed")
	}
}
