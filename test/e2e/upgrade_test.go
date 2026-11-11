package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// The upgrade tests run the released binary of the previous release
// (kudurrutest.Previous, v0.1.0) on a data directory, stop it, and start the
// binary under test on the same directory with the same configuration file
// and environment: what an operator does when the image tag changes (D373).

// upgradePassword is the admin password of the upgrade fixtures: a synthetic
// value, so that no generated credential appears in a failure log.
const upgradePassword = "upgrade-e2e-password-1" // #nosec G101 -- disposable loopback test credential

// envWriteV01Fixtures names a directory that receives the state.db files the
// previous release wrote in these tests, gzip-compressed: the golden files of
// internal/store/sqlite/testdata/v0.1.0 (hack/upgrade-fixture.sh).
const envWriteV01Fixtures = "KUDURRU_E2E_WRITE_V01_FIXTURES"

// The schema versions the 0.1.0 release wrote (meta.schema_version of each
// database). A fixture with other values was not written by that release.
var v01Schemas = map[string]string{"state.db": "1", "local.db": "1", "querylog.db": "2"}

// currentSchemas are the versions the binary under test writes.
func currentSchemas() map[string]string {
	return map[string]string{
		"state.db":    strconv.Itoa(store.SchemaVersion),
		"local.db":    strconv.Itoa(store.SchemaVersion),
		"querylog.db": strconv.Itoa(sqlitelog.SchemaVersion),
	}
}

// schemaVersions reads meta.schema_version of the databases of a stopped
// node.
func schemaVersions(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"state.db", localdb.FileName, "querylog.db"} {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, name)+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var v any
		err = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
		_ = db.Close()
		if err != nil {
			t.Fatalf("%s: schema_version: %v", name, err)
		}
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		out[name] = fmt.Sprint(v)
	}
	return out
}

// stateRows counts rows of a stopped node's state.db.
func stateRows(t *testing.T, dataDir, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "state.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("state.db: %s: %v", query, err)
	}
	return n
}

// settle waits until a node that just became ready has finished the writes
// it makes of its own accord after a start. Readiness comes when every zone
// is admitted; the primary runtime then still commits zone lifecycles and
// DNSZone status for a moment (about half a second after an upgrade on an
// idle machine, longer under load). Each such commit publishes a new
// serving generation, and an API write whose transaction overlaps one is
// answered 503 `unavailable` without effect, while a query in flight at the
// switch is dropped (docs/architecture/query-path.md). The test asks its
// questions and makes its writes once readiness has held, and state.db has
// not changed, for a second: a write is made once and its first answer is
// the one asserted (D373).
func settle(t *testing.T, spec kudurrutest.NodeSpec) {
	t.Helper()
	revision := func() string {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(spec.DataDir, "state.db")+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var v any
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'resource_version'`).Scan(&v); err != nil {
			return "unreadable: " + err.Error()
		}
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		return fmt.Sprint(v)
	}
	start := time.Now()
	last, quiet := "", 0
	if !kudurrutest.Eventually(60*time.Second, 100*time.Millisecond, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		code, _, err := kudurrutest.Get(ctx, "http://"+spec.Ports.Ops+"/healthz/ready")
		now := revision()
		if err != nil || code != http.StatusOK || now != last {
			last, quiet = now, 0
			return false
		}
		quiet++
		return quiet >= 10
	}) {
		t.Fatalf("the node did not settle within 60s (store revision %s)", last)
	}
	t.Logf("timing: settled %s after readiness, at store revision %s", time.Since(start).Round(time.Millisecond), last)
}

// upgradeAPI calls the API of one node with a bearer token.
type upgradeAPI struct{ addr, token string }

func (c upgradeAPI) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.addr+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b
}

// must is do for a call that has one acceptable status.
func (c upgradeAPI) must(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	code, b := c.do(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, code, want, b)
	}
	return b
}

// identities lists the objects of the public kinds: "<kind>/<namespace>/<name>"
// to "<uid> generation <n>".
func (c upgradeAPI) identities(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kind := range []string{"dnszones", "dnsrecordsets", "blocklists", "clientgroups", "policies"} {
		var list struct {
			Items []struct {
				Metadata struct {
					Name, Namespace, UID string
					Generation           int64
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(c.must(t, http.MethodGet, "/objects/"+kind, nil, http.StatusOK), &list); err != nil {
			t.Fatalf("list %s: %v", kind, err)
		}
		for _, it := range list.Items {
			out[kind+"/"+it.Metadata.Namespace+"/"+it.Metadata.Name] = fmt.Sprintf("%s generation %d", it.Metadata.UID, it.Metadata.Generation)
		}
	}
	return out
}

// principal is the identity GET /auth/me reports for the token, with its
// roles: "<kind> <name> [roles]".
func (c upgradeAPI) principal(t *testing.T) string {
	t.Helper()
	code, b := c.do(t, http.MethodGet, "/auth/me", nil)
	if code != http.StatusOK {
		return fmt.Sprintf("%d %s", code, b)
	}
	var me struct {
		Principal struct{ Kind, Name string } `json:"principal"`
		Roles     []string                    `json:"roles"`
	}
	if err := json.Unmarshal(b, &me); err != nil {
		t.Fatal(err)
	}
	sort.Strings(me.Roles)
	return fmt.Sprintf("%s %s %v", me.Principal.Kind, me.Principal.Name, me.Roles)
}

// adminSocketToken mints a token with `<bin> admin create-token` over the
// node's admin socket.
func adminSocketToken(t *testing.T, bin string, spec kudurrutest.NodeSpec, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"admin", "create-token", "--socket", filepath.Join(spec.DataDir, "admin.sock")}, args...)...) //nolint:gosec // the test's binary
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s admin create-token %v: %v", filepath.Base(bin), args, err)
	}
	token := strings.TrimSpace(string(out))
	if !strings.HasPrefix(token, "kdr_") {
		t.Fatalf("admin create-token printed no token")
	}
	return token
}

// passwordLogin logs in as admin and returns the roles of the session.
func passwordLogin(t *testing.T, addr string) (int, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": upgradePassword})
	resp, err := client.Post("http://"+addr+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ""
	}
	var session struct {
		Principal struct{ Kind, Name string } `json:"principal"`
		Roles     []string                    `json:"roles"`
	}
	if err := json.Unmarshal(b, &session); err != nil {
		t.Fatal(err)
	}
	sort.Strings(session.Roles)
	return resp.StatusCode, fmt.Sprintf("%s %s %v", session.Principal.Kind, session.Principal.Name, session.Roles)
}

// ask sends one question and repeats it while it goes unanswered. A query
// that is in flight when a configuration generation is published gets no
// authoritative answer from the retired generation
// (docs/architecture/query-path.md); the stub resolver's retry is part of
// the protocol, and these tests commit and query in quick succession.
func ask(t *testing.T, addr, name string, qtype uint16, tcp bool) *dns.Msg {
	t.Helper()
	var err error
	for range 4 {
		var r *dns.Msg
		if r, err = exchange(addr, name, qtype, tcp); err == nil {
			return r
		}
	}
	t.Fatalf("%s %s (tcp %v): no answer: %v", name, dns.TypeToString[qtype], tcp, err)
	return nil
}

// upgradeQuestion is one question of the comparison across the upgrade.
type upgradeQuestion struct {
	name  string
	qtype uint16
}

// upgradeQuestions cover file-managed and API-managed authoritative data, a
// name that does not exist, the apex NS, list blocking, the personal allow
// and deny lists, and forwarding.
var upgradeQuestions = []upgradeQuestion{
	{"host.file.test.", dns.TypeA}, {"ns1.file.test.", dns.TypeA}, {"file.test.", dns.TypeNS},
	{"www.api.test.", dns.TypeA}, {"ns1.api.test.", dns.TypeA}, {"txt.api.test.", dns.TypeTXT},
	{"missing.api.test.", dns.TypeA}, {"www.api.test.", dns.TypeAAAA},
	{"ads.blocked.test.", dns.TypeA}, {"allowed.blocked.test.", dns.TypeA}, {"denied.upgrade.test.", dns.TypeA},
	{"forwarded.example.", dns.TypeA},
	// The record set with the endpoints the zone builder refuses: what
	// 0.1.0 served of it, and that the refused ones were left out.
	{"kept.api.test.", dns.TypeA}, {"api.test.", dns.TypeNS}, {"api.test.", dns.TypeCNAME}, {"www.api.test.", dns.TypeCNAME},
	// The record sets that are invalid as a whole, and the CNAME and A
	// record for one name.
	{"bad-a.api.test.", dns.TypeA}, {"twice.api.test.", dns.TypeA}, {"mail.api.test.", dns.TypeMX},
	{"pair.api.test.", dns.TypeA}, {"pair.api.test.", dns.TypeCNAME},
}

// answers asks every upgrade question over UDP and TCP and renders what a
// client sees of each: the response code, the AA bit and the answer records
// without their TTL (a forwarded answer counts its TTL down).
func answers(t *testing.T, addr string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, q := range upgradeQuestions {
		for _, tcp := range []bool{false, true} {
			r := ask(t, addr, q.name, q.qtype, tcp)
			var rrs []string
			for _, rr := range r.Answer {
				rr = dns.Copy(rr)
				rr.Header().Ttl = 0
				rrs = append(rrs, rr.String())
			}
			sort.Strings(rrs)
			proto := "udp"
			if tcp {
				proto = "tcp"
			}
			out[fmt.Sprintf("%s %s %s", q.name, dns.TypeToString[q.qtype], proto)] = fmt.Sprintf("%s aa=%v %v", dns.RcodeToString[r.Rcode], r.Authoritative, rrs)
		}
	}
	return out
}

// soaSerial returns the serial of the zone's SOA.
func soaSerial(t *testing.T, addr, zone string) uint32 {
	t.Helper()
	r := ask(t, addr, zone, dns.TypeSOA, false)
	if len(r.Answer) == 0 {
		t.Fatalf("%s SOA: %s", zone, dns.RcodeToString[r.Rcode])
	}
	soa, ok := r.Answer[0].(*dns.SOA)
	if !ok {
		t.Fatalf("%s SOA: %v", zone, r.Answer[0])
	}
	return soa.Serial
}

// v01Directory is a node of the previous release with its data directory,
// its credentials and what it answered before it stopped.
type v01Directory struct {
	previous string // the binary of the previous release
	spec     kudurrutest.NodeSpec
	env      []string
	upstream *kudurrutest.Upstream

	breakGlass, personal, bootstrap string // API tokens minted by the previous release
	principals                      map[string]string
	answers                         map[string]string
	serials                         map[string]uint32
	identities                      map[string]string
	queryIDs                        []int64
	queryNames                      []string
	auditRows                       int
}

func (d *v01Directory) api(token string) upgradeAPI {
	return upgradeAPI{addr: d.spec.Ports.API, token: token}
}

func (d *v01Directory) options() kudurrutest.Options { return kudurrutest.Options{Env: d.env} }

// writeV01Directory starts the previous release on a new data directory and
// uses it like an operator of that release: zones from a file and through
// the API, a block list, a client group, the default policy, personal allow
// and deny entries, a setting saved from the Settings page, API tokens, a
// password login and queries. legacy also stores the objects 0.1.0 accepted
// and this release cannot serve. The node keeps running; stop() stops it and
// records what the databases hold.
func writeV01Directory(t *testing.T, name string, legacy bool) (*v01Directory, *kudurrutest.Node) {
	t.Helper()
	d := &v01Directory{previous: kudurrutest.Previous(t)}
	dir := t.TempDir()
	d.upstream = kudurrutest.StartUpstream(t, map[string]string{"forwarded.example.": "198.51.100.20", "allowed.blocked.test.": "198.51.100.30"})
	spec, cfg := kudurrutest.NewNodeSpec(t, dir, name)
	d.spec = spec
	objDir := filepath.Join(dir, "objects.d")
	if err := os.MkdirAll(objDir, 0o750); err != nil {
		t.Fatal(err)
	}
	var file strings.Builder
	for _, doc := range []any{
		map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "file-test"},
			"spec": map[string]any{
				"zoneName": "file.test.", "type": "primary", "nameservers": []any{"ns1.file.test."},
				"soa": map[string]any{"mname": "ns1.file.test.", "rname": "hostmaster.file.test."},
			},
		},
		map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": map[string]any{"name": "hosts", "namespace": "default"},
			"spec": map[string]any{"zoneRef": map[string]any{"name": "file-test"}, "endpoints": []any{
				map[string]any{"dnsName": "ns1.file.test", "recordType": "A", "targets": []any{"127.0.0.1"}},
				map[string]any{"dnsName": "host.file.test", "recordType": "A", "targets": []any{"192.0.2.1"}},
			}},
		},
	} {
		b, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		file.WriteString("---\n")
		file.Write(b)
	}
	if err := os.WriteFile(filepath.Join(objDir, "zone.yaml"), []byte(file.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg["objectsDir"] = objDir
	cfg["settingsFile"] = filepath.Join(dir, "settings.yaml")
	cfg["upstreams"].(map[string]any)["groups"] = map[string]any{
		"default": map[string]any{"servers": []any{d.upstream.Addr}, "strategy": "strict", "timeout": "2s"},
	}
	// exclude.sudn is on by default and would drop the .test names (RFC 6761).
	cfg["querylog"] = map[string]any{"enabled": true, "sink": "sqlite", "exclude": map[string]any{"sudn": false}}
	cfg["auth"].(map[string]any)["session"] = map[string]any{"secure": "never"}
	kudurrutest.WriteConfig(t, spec.Config, cfg)

	// A bootstrap token as the Helm chart installs one: generated offline
	// and handed to every start in the environment.
	generated, err := exec.Command(d.previous, "token", "generate").Output() //nolint:gosec // the test's binary
	if err != nil {
		t.Fatalf("token generate: %v", err)
	}
	d.bootstrap = strings.TrimSpace(string(generated))
	d.env = []string{"KUDURRU_ADMIN_PASSWORD=" + upgradePassword, "KUDURRU_BOOTSTRAP_TOKEN=" + d.bootstrap}

	// The 0.1.0 image had no default command; serve is named explicitly.
	node := kudurrutest.StartReady(t, d.previous, spec, kudurrutest.Options{Env: d.env, Args: []string{"serve", "--config", spec.Config}})
	if log := node.Log(); !strings.Contains(log, "version="+kudurrutest.PreviousRelease) || !strings.Contains(log, "first boot: seeded defaults") {
		t.Fatalf("the first run is not a first boot of %s:\n%s", kudurrutest.PreviousRelease, log)
	}
	d.breakGlass = adminSocketToken(t, d.previous, spec, "--description", "break-glass", "--expires-in", "720h")
	d.personal = adminSocketToken(t, d.previous, spec, "--user", "admin", "--description", "personal")
	api := d.api(d.breakGlass)

	object := func(kind, name, namespace string, spec map[string]any) map[string]any {
		meta := map[string]any{"name": name}
		if namespace != "" {
			meta["namespace"] = namespace
		}
		return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": kind, "metadata": meta, "spec": spec}
	}
	api.must(t, http.MethodPost, "/objects/dnszones", object("DNSZone", "api-test", "", map[string]any{
		"zoneName": "api.test.", "type": "primary", "nameservers": []any{"ns1.api.test."},
		"soa": map[string]any{"mname": "ns1.api.test.", "rname": "hostmaster.api.test."},
	}), http.StatusCreated)
	api.must(t, http.MethodPost, "/objects/dnszones", object("DNSZone", "corp", "", map[string]any{
		"zoneName": "corp.test.", "type": "forward", "forward": map[string]any{"upstreamGroup": "default"},
	}), http.StatusCreated)
	// A record set that names a forward zone: 0.1.0 stored it and reported
	// it on the record set; it must not stop this release.
	api.must(t, http.MethodPost, "/objects/dnsrecordsets", object("DNSRecordSet", "corp-local", "default", map[string]any{
		"zoneRef": map[string]any{"name": "corp"}, "endpoints": []any{map[string]any{"dnsName": "www.corp.test", "recordType": "A", "targets": []any{"192.0.2.99"}}},
	}), http.StatusCreated)
	api.must(t, http.MethodPost, "/objects/blocklists", object("BlockList", "ads", "default", map[string]any{
		"sources": []any{map[string]any{"name": "inline", "format": "domains", "inline": []any{"ads.blocked.test", "allowed.blocked.test"}}},
	}), http.StatusCreated)
	api.must(t, http.MethodPost, "/objects/clientgroups", object("ClientGroup", "lan", "default", map[string]any{
		"clients": []any{map[string]any{"cidr": "127.0.0.0/8"}},
	}), http.StatusCreated)
	// The zone is listed once the controller has seen it.
	record := func(dnsName, recordType string, targets ...any) {
		t.Helper()
		var last string
		if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool {
			code, b := api.do(t, http.MethodPost, "/zones/api-test/records", map[string]any{"dnsName": dnsName, "recordType": recordType, "targets": targets})
			last = fmt.Sprintf("%d %s", code, b)
			return code == http.StatusCreated || code == http.StatusOK
		}) {
			t.Fatalf("add %s %s to api.test: %s", dnsName, recordType, last)
		}
	}
	record("ns1.api.test", "A", "127.0.0.1")
	record("www.api.test", "A", "192.0.2.10")
	record("txt.api.test", "TXT", "written by 0.1.0")
	// A record set with endpoints the zone builder refuses next to one it
	// serves: a CNAME and an NS endpoint at the apex and a CNAME next to the
	// A record of www. 0.1.0 stored it, reported the refused endpoints on
	// it and served the rest of the zone (D373).
	api.must(t, http.MethodPost, "/objects/dnsrecordsets", object("DNSRecordSet", "refused-endpoints", "default", map[string]any{
		"zoneRef": map[string]any{"name": "api-test"}, "endpoints": []any{
			map[string]any{"dnsName": "api.test", "recordType": "CNAME", "targets": []any{"elsewhere.test."}},
			map[string]any{"dnsName": "api.test", "recordType": "NS", "targets": []any{"ns2.elsewhere.test."}},
			map[string]any{"dnsName": "www.api.test", "recordType": "CNAME", "targets": []any{"elsewhere.test."}},
			map[string]any{"dnsName": "kept.api.test", "recordType": "A", "targets": []any{"192.0.2.77"}},
		},
	}), http.StatusCreated)
	// Record sets that are invalid as a whole, which 0.1.0 stored and
	// reported Invalid, serving the rest of the zone; and a CNAME and an A
	// record for one name in one record set, of which 0.1.0 served the
	// CNAME (D373).
	for name, endpoints := range map[string][]any{
		"v01-not-an-address": {map[string]any{"dnsName": "bad-a.api.test", "recordType": "A", "targets": []any{"not-an-ip"}}},
		"v01-two-for-a-name": {map[string]any{"dnsName": "twice.api.test", "recordType": "A", "targets": []any{"192.0.2.81"}}, map[string]any{"dnsName": "twice.api.test", "recordType": "A", "targets": []any{"192.0.2.82"}}},
		"v01-unknown-type":   {map[string]any{"dnsName": "bogus.api.test", "recordType": "BOGUS", "targets": []any{"x"}}},
		"v01-mx-no-pref":     {map[string]any{"dnsName": "mail.api.test", "recordType": "MX", "targets": []any{"mx.api.test."}}},
		"v01-cname-and-a":    {map[string]any{"dnsName": "pair.api.test", "recordType": "CNAME", "targets": []any{"elsewhere.test."}}, map[string]any{"dnsName": "pair.api.test", "recordType": "A", "targets": []any{"192.0.2.83"}}},
	} {
		api.must(t, http.MethodPost, "/objects/dnsrecordsets", object("DNSRecordSet", name, "default", map[string]any{
			"zoneRef": map[string]any{"name": "api-test"}, "endpoints": endpoints,
		}), http.StatusCreated)
	}
	// A block list whose URL never downloaded: 0.1.0 stored it, reported
	// the failed fetch on it and served it empty. The server answers 404;
	// with the default blocking.allowLocalListURLs both releases refuse the
	// loopback URL before they ask it, which is the same published failure.
	deadList := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(deadList.Close)
	api.must(t, http.MethodPost, "/objects/blocklists", object("BlockList", "dead-url", "default", map[string]any{
		"sources": []any{map[string]any{"name": "remote", "format": "domains", "url": deadList.URL + "/never.txt"}},
	}), http.StatusCreated)
	if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
		var list struct {
			Status struct {
				Sources []struct {
					Digest string `json:"digest"`
					Error  string `json:"error"`
				} `json:"sources"`
			} `json:"status"`
		}
		if err := json.Unmarshal(api.must(t, http.MethodGet, "/objects/blocklists/default/dead-url", nil, http.StatusOK), &list); err != nil {
			t.Fatal(err)
		}
		return len(list.Status.Sources) == 1 && list.Status.Sources[0].Digest == "" && list.Status.Sources[0].Error != ""
	}) {
		t.Fatalf("%s did not report the failed fetch of the block list", kudurrutest.PreviousRelease)
	}
	// The community license has one policy: the default one gets the list.
	var policy map[string]any
	if err := json.Unmarshal(api.must(t, http.MethodGet, "/objects/policies/default/default", nil, http.StatusOK), &policy); err != nil {
		t.Fatal(err)
	}
	delete(policy, "status")
	policySpec := policy["spec"].(map[string]any)
	policySpec["blockListRefs"] = append(policySpec["blockListRefs"].([]any), "ads")
	if legacy {
		// Rewrites were a placeholder in 0.1.0: any item with a name passed.
		policySpec["rewrites"] = []any{map[string]any{"name": "placeholder.rewrite.test"}}
	}
	api.must(t, http.MethodPut, "/objects/policies/default/default", policy, http.StatusOK)
	api.must(t, http.MethodPost, "/allowlist", map[string]any{"values": []any{"allowed.blocked.test"}, "comment": "upgrade"}, http.StatusOK)
	api.must(t, http.MethodPost, "/denylist", map[string]any{"values": []any{"denied.upgrade.test"}, "comment": "upgrade"}, http.StatusOK)
	// A change on the Settings page: 0.1.0 saved it in the settings file.
	api.must(t, http.MethodPatch, "/settings", map[string]any{"cache.minTTL": "7s"}, http.StatusOK)
	if b, err := os.ReadFile(cfg["settingsFile"].(string)); err != nil || !strings.Contains(string(b), "minTTL: 7s") { //nolint:gosec // the test's file
		t.Fatalf("0.1.0 did not save the setting in the settings file: %s %v", b, err)
	}
	if code, who := passwordLogin(t, spec.Ports.API); code != http.StatusOK || who != "User admin [admin]" {
		t.Fatalf("password login on %s: %d %s", kudurrutest.PreviousRelease, code, who)
	}
	d.principals = map[string]string{}
	for label, token := range map[string]string{"break-glass": d.breakGlass, "personal": d.personal, "bootstrap": d.bootstrap} {
		d.principals[label] = d.api(token).principal(t)
		if !strings.Contains(d.principals[label], "[admin]") {
			t.Fatalf("%s token on %s: %s", label, kudurrutest.PreviousRelease, d.principals[label])
		}
	}
	// The lists compile and the zones are stamped a moment after the writes.
	if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
		blocked, err := exchange(spec.Ports.DNS, "ads.blocked.test.", dns.TypeA, false)
		denied, err2 := exchange(spec.Ports.DNS, "denied.upgrade.test.", dns.TypeA, false)
		allowed, err3 := exchange(spec.Ports.DNS, "allowed.blocked.test.", dns.TypeA, false)
		text, err4 := exchange(spec.Ports.DNS, "txt.api.test.", dns.TypeTXT, false)
		return err == nil && err2 == nil && err3 == nil && err4 == nil && firstA(blocked) == "0.0.0.0" && firstA(denied) == "0.0.0.0" &&
			firstA(allowed) == "198.51.100.30" && len(text.Answer) == 1
	}) {
		t.Fatalf("%s did not serve what was configured", kudurrutest.PreviousRelease)
	}
	// The controller stamps the serial a moment after the content changed:
	// the serials of the directory are the ones it settled on, stamped in
	// the zone status and served.
	stamped := func() map[string]uint32 {
		var zones struct {
			Items []struct {
				ZoneName string `json:"zoneName"`
				Serial   uint32 `json:"serial"`
			} `json:"items"`
		}
		if err := json.Unmarshal(api.must(t, http.MethodGet, "/zones", nil, http.StatusOK), &zones); err != nil {
			t.Fatal(err)
		}
		out := map[string]uint32{}
		for _, z := range zones.Items {
			out[z.ZoneName] = z.Serial
		}
		return out
	}
	settled := 0
	if !kudurrutest.Eventually(30*time.Second, 250*time.Millisecond, func() bool {
		now, status := map[string]uint32{}, stamped()
		for _, zone := range []string{"file.test.", "api.test."} {
			now[zone] = soaSerial(t, spec.Ports.DNS, zone)
			if now[zone] != status[zone] || now[zone] < 2 {
				settled = 0
				return false
			}
		}
		if maps.Equal(now, d.serials) {
			settled++
		} else {
			d.serials, settled = now, 0
		}
		return settled >= 4
	}) {
		t.Fatalf("the zone serials of %s did not settle: %v", kudurrutest.PreviousRelease, d.serials)
	}
	if legacy {
		// 0.1.0 accepted these and reported "zone type ... is not served
		// by this version" on each: the types it reserved for later, with or
		// without their configuration, and a second DNSZone for a zone name.
		// They are stored last: from the moment a zone name has two objects,
		// 0.1.0 keeps serving the zone as it was, so the zone is complete
		// and its serial settled first.
		legacyZones := map[string]map[string]any{
			"legacy-secondary": {"zoneName": "secondary.test.", "type": "secondary"},
			"legacy-stub":      {"zoneName": "stub.test.", "type": "stub"},
			"legacy-external":  {"zoneName": "external.test.", "type": "external"},
			"legacy-duplicate": {"zoneName": "API.test.", "type": "primary"},
			// Update grants were a placeholder too: 0.1.0 stored and
			// served these zones, and the primary runtime of this release
			// cannot compile their policies.
			"legacy-grant-outside": {"zoneName": "grant-outside.test.", "type": "primary", "nameservers": []any{"ns1.grant-outside.test."},
				"policy": map[string]any{"update": "deny", "updatePolicies": []any{map[string]any{"tsigKeyRef": "ddns", "name": "host.elsewhere.test."}}}},
			"legacy-grant-type": {"zoneName": "grant-type.test.", "type": "primary", "nameservers": []any{"ns1.grant-type.test."},
				"policy": map[string]any{"update": "allow", "updatePolicies": []any{map[string]any{"tsigKeyRef": "ddns", "name": "grant-type.test.", "types": []any{"BOGUS"}}}}},
		}
		for zoneName, spec := range legacyZones {
			api.must(t, http.MethodPost, "/objects/dnszones", object("DNSZone", zoneName, "", spec), http.StatusCreated)
		}
		// The controller of 0.1.0 has seen and judged them.
		var last string
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			var zones struct {
				Items []struct {
					Name   string `json:"name"`
					Reason string `json:"reason"`
				} `json:"items"`
			}
			b := api.must(t, http.MethodGet, "/zones", nil, http.StatusOK)
			if err := json.Unmarshal(b, &zones); err != nil {
				t.Fatal(err)
			}
			last = string(b)
			unsupported := 0
			for _, z := range zones.Items {
				if _, ok := legacyZones[z.Name]; ok && z.Reason == "Unsupported" {
					unsupported++
				}
			}
			return unsupported == 3
		}) {
			t.Fatalf("%s did not report the zones it does not serve: %s", kudurrutest.PreviousRelease, last)
		}
	}
	d.answers = answers(t, spec.Ports.DNS)
	for q, want := range map[string]string{
		"host.file.test. A udp":       "NOERROR aa=true [host.file.test.\t0\tIN\tA\t192.0.2.1]",
		"www.api.test. A tcp":         "NOERROR aa=true [www.api.test.\t0\tIN\tA\t192.0.2.10]",
		"missing.api.test. A udp":     "NXDOMAIN aa=true []",
		"ads.blocked.test. A udp":     "NOERROR aa=false [ads.blocked.test.\t0\tIN\tA\t0.0.0.0]",
		"forwarded.example. A tcp":    "NOERROR aa=false [forwarded.example.\t0\tIN\tA\t198.51.100.20]",
		"allowed.blocked.test. A udp": "NOERROR aa=false [allowed.blocked.test.\t0\tIN\tA\t198.51.100.30]",
		"kept.api.test. A udp":        "NOERROR aa=true [kept.api.test.\t0\tIN\tA\t192.0.2.77]",
	} {
		if d.answers[q] != want {
			t.Fatalf("%s answered %s with %q, want %q", kudurrutest.PreviousRelease, q, d.answers[q], want)
		}
	}
	d.identities = api.identities(t)
	return d, node
}

// stop stops the node of the previous release and records what its
// databases hold. The files must be the ones 0.1.0 writes.
func (d *v01Directory) stop(t *testing.T, node *kudurrutest.Node) {
	t.Helper()
	node.Stop(20 * time.Second)
	if code, err := node.Wait(time.Second); err != nil || code != 0 {
		t.Fatalf("%s did not stop cleanly: exit %d, %v\n%s", kudurrutest.PreviousRelease, code, err, node.Log())
	}
	if got := schemaVersions(t, d.spec.DataDir); !maps.Equal(got, v01Schemas) {
		t.Fatalf("the data directory does not have the schemas of %s: %v, want %v", kudurrutest.PreviousRelease, got, v01Schemas)
	}
	d.queryIDs, d.queryNames = queryLogRows(t, d.spec.DataDir)
	if !containsName(d.queryNames, "host.file.test") || !containsName(d.queryNames, "forwarded.example") {
		t.Fatalf("the query log of %s lacks the queries: %v", kudurrutest.PreviousRelease, d.queryNames)
	}
	d.auditRows = stateRows(t, d.spec.DataDir, `SELECT count(*) FROM audit`)
	if d.auditRows == 0 {
		t.Fatalf("%s wrote no audit rows", kudurrutest.PreviousRelease)
	}
}

// writeFixture copies the state.db the previous release wrote into the
// directory $KUDURRU_E2E_WRITE_V01_FIXTURES names, when it is set.
func (d *v01Directory) writeFixture(t *testing.T, name string) {
	t.Helper()
	dst := os.Getenv(envWriteV01Fixtures)
	if dst == "" {
		return
	}
	path := filepath.Join(d.spec.DataDir, "state.db")
	// A clean stop checkpoints the write-ahead log into the database file.
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
		t.Fatalf("%s-wal still holds %d bytes after a clean stop", path, info.Size())
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the test's data directory
	if err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	w, err := gzip.NewWriterLevel(&packed, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dst, name)
	if err := os.WriteFile(out, packed.Bytes(), 0o644); err != nil { //nolint:gosec // G306: a committed test fixture
		t.Fatal(err)
	}
	t.Logf("wrote %s: %d bytes of state.db (%d rows of audit) as %d bytes", out, len(raw), d.auditRows, packed.Len())
}

// TestUpgradeFromV01DataDirectory upgrades a data directory written by the
// released 0.1.0 binary with the binary under test (KUDURRU_E2E_BINARY when
// set): the node becomes ready without a first boot, gives every client the
// answers 0.1.0 gave, keeps object identities, zone serials, credentials,
// the saved setting, the query log and the audit trail, stays that way
// across restarts, and the 0.1.0 binary then refuses the directory without
// touching it.
func TestUpgradeFromV01DataDirectory(t *testing.T) {
	kudurrutest.RequireE2E(t)
	d, old := writeV01Directory(t, "upgrade", false)
	d.stop(t, old)
	d.writeFixture(t, "state.db.gz")
	spec := d.spec

	start := time.Now()
	node := kudurrutest.StartReady(t, kudurruBin, spec, d.options())
	t.Logf("timing: upgraded and ready in %s", time.Since(start).Round(time.Millisecond))
	settle(t, spec)
	api := d.api(d.breakGlass)

	t.Run("NoFirstBoot", func(t *testing.T) {
		log := node.Log()
		for _, s := range []string{"first boot: seeded defaults", "created the first-boot admin", "bootstrap: created", "generated the encryption key"} {
			if strings.Contains(log, s) {
				t.Errorf("the upgrade logged %q: the data directory was not reused", s)
			}
		}
	})
	t.Run("SameAnswers", func(t *testing.T) {
		// The served data follows the store a moment after readiness.
		var got map[string]string
		var diff []string
		if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool {
			got, diff = answers(t, spec.Ports.DNS), nil
			for q, want := range d.answers {
				if got[q] != want {
					diff = append(diff, fmt.Sprintf("%s: %q, %s answered %q", q, got[q], kudurrutest.PreviousRelease, want))
				}
			}
			return len(diff) == 0
		}) {
			sort.Strings(diff)
			t.Fatalf("answers changed across the upgrade:\n%s", strings.Join(diff, "\n"))
		}
	})
	serials := map[string]uint32{}
	t.Run("SerialsContinue", func(t *testing.T) {
		for zone, before := range d.serials {
			serials[zone] = soaSerial(t, spec.Ports.DNS, zone)
			t.Logf("%s serial %d on %s, %d after the upgrade", zone, before, kudurrutest.PreviousRelease, serials[zone])
			// The content did not change, so neither does the serial: a
			// secondary of the zone has nothing to transfer after the upgrade.
			if serials[zone] != before {
				t.Errorf("%s serial %d after the upgrade, %d on %s (unchanged content)", zone, serials[zone], before, kudurrutest.PreviousRelease)
			}
		}
	})
	t.Run("ObjectIdentities", func(t *testing.T) {
		got := api.identities(t)
		for key, want := range d.identities {
			if got[key] != want {
				t.Errorf("%s: %q, was %q on %s", key, got[key], want, kudurrutest.PreviousRelease)
			}
		}
		if len(got) != len(d.identities) {
			t.Errorf("%d objects after the upgrade, %d before", len(got), len(d.identities))
		}
	})
	t.Run("CredentialsOfThePreviousRelease", func(t *testing.T) {
		for label, token := range map[string]string{"break-glass": d.breakGlass, "personal": d.personal, "bootstrap": d.bootstrap} {
			if got := d.api(token).principal(t); got != d.principals[label] {
				t.Errorf("%s token: %s, was %s on %s", label, got, d.principals[label], kudurrutest.PreviousRelease)
			}
		}
		if code, who := passwordLogin(t, spec.Ports.API); code != http.StatusOK || who != "User admin [admin]" {
			t.Errorf("password login after the upgrade: %d %s", code, who)
		}
		if code, _ := d.api("kdr_unknown_unknown").do(t, http.MethodGet, "/auth/me", nil); code != http.StatusUnauthorized {
			t.Errorf("an unknown token: %d, want 401", code)
		}
	})
	minTTL := func(t *testing.T) string {
		t.Helper()
		var settings struct {
			Settings struct {
				Cache struct {
					MinTTL string `json:"minTTL"`
				} `json:"cache"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(d.api(d.breakGlass).must(t, http.MethodGet, "/settings", nil, http.StatusOK), &settings); err != nil {
			t.Fatal(err)
		}
		return settings.Settings.Cache.MinTTL
	}
	t.Run("SavedSetting", func(t *testing.T) {
		if got := minTTL(t); got != "7s" {
			t.Errorf("cache.minTTL = %q after the upgrade, %s had saved 7s", got, kudurrutest.PreviousRelease)
		}
		// Cluster settings now live in the store: a change is kept there,
		// and the settings file of 0.1.0 does not take it back at a restart
		// (checked after the restarts below).
		api.must(t, http.MethodPatch, "/settings", map[string]any{"cache.minTTL": "9s"}, http.StatusOK)
	})
	t.Run("WritesAfterTheUpgrade", func(t *testing.T) {
		api.must(t, http.MethodPost, "/objects/dnszones", map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "new-test"},
			"spec": map[string]any{"zoneName": "new.test.", "type": "primary", "nameservers": []any{"ns1.new.test."}},
		}, http.StatusCreated)
		var last string
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			code, b := api.do(t, http.MethodPost, "/zones/new-test/records", map[string]any{"dnsName": "www.new.test", "recordType": "A", "targets": []any{"192.0.2.77"}})
			last = fmt.Sprintf("%d %s", code, b)
			return code == http.StatusCreated || code == http.StatusOK
		}) {
			t.Fatalf("add a record after the upgrade: %s", last)
		}
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			r, err := exchange(spec.Ports.DNS, "www.new.test.", dns.TypeA, false)
			return err == nil && firstA(r) == "192.0.2.77"
		}) {
			t.Fatal("a zone created after the upgrade is not served")
		}
		// What 0.1.0 stored and this release refuses to store.
		code, b := api.do(t, http.MethodPost, "/objects/dnszones", map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "again"},
			"spec": map[string]any{"zoneName": "api.test.", "type": "primary"},
		})
		if code != http.StatusBadRequest || !strings.Contains(string(b), "api-test") {
			t.Errorf("a second DNSZone for api.test.: %d %s", code, b)
		}
	})
	node.Stop(20 * time.Second)
	if code, err := node.Wait(time.Second); err != nil || code != 0 {
		t.Fatalf("the upgraded node did not stop cleanly: exit %d, %v", code, err)
	}

	t.Run("Schemas", func(t *testing.T) {
		if got, want := schemaVersions(t, spec.DataDir), currentSchemas(); !maps.Equal(got, want) {
			t.Errorf("schema versions after the upgrade: %v, want %v", got, want)
		}
	})
	t.Run("QueryLogContinuity", func(t *testing.T) {
		ids, names := queryLogRows(t, spec.DataDir)
		if len(ids) <= len(d.queryIDs) {
			t.Fatalf("query log rows: %d after the upgrade, %d before", len(ids), len(d.queryIDs))
		}
		for i := range d.queryIDs {
			if ids[i] != d.queryIDs[i] || names[i] != d.queryNames[i] {
				t.Fatalf("row %d of %s changed: %d %s -> %d %s", i, kudurrutest.PreviousRelease, d.queryIDs[i], d.queryNames[i], ids[i], names[i])
			}
		}
		if ids[len(d.queryIDs)] <= d.queryIDs[len(d.queryIDs)-1] || !containsName(names[len(d.queryIDs):], "www.new.test") {
			t.Errorf("rows after the upgrade: ids from %d (last of %s: %d), names %v", ids[len(d.queryIDs)], kudurrutest.PreviousRelease, d.queryIDs[len(d.queryIDs)-1], names[len(d.queryIDs):])
		}
	})
	// Kudurru/default is created once, from the effective settings of the
	// first start of this release, and not again.
	settingsObject := func(t *testing.T) string {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+filepath.Join(spec.DataDir, "state.db")+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var uid, created string
		var n int
		if err := db.QueryRow(`SELECT count(*), coalesce(max(uid), ''), coalesce(max(created_at), '') FROM objects WHERE kind = 'Kudurru'`).Scan(&n, &uid, &created); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%d object, uid %s, created %s", n, uid, created)
	}
	settingsCreated := settingsObject(t)
	if !strings.HasPrefix(settingsCreated, "1 object, uid ") || strings.Contains(settingsCreated, "uid ,") {
		t.Fatalf("Kudurru/default after the upgrade: %s", settingsCreated)
	}

	for run := 2; run <= 3; run++ {
		t.Run(fmt.Sprintf("Restart%d", run), func(t *testing.T) {
			n := kudurrutest.StartReady(t, kudurruBin, spec, d.options())
			settle(t, spec)
			for _, s := range []string{"first boot: seeded defaults", "bootstrap: created"} {
				if strings.Contains(n.Log(), s) {
					t.Errorf("restart logged %q", s)
				}
			}
			var diff []string
			if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool {
				got := answers(t, spec.Ports.DNS)
				diff = nil
				for q, want := range d.answers {
					if got[q] != want {
						diff = append(diff, fmt.Sprintf("%s: %q, want %q", q, got[q], want))
					}
				}
				return len(diff) == 0
			}) {
				t.Fatalf("answers after the restart:\n%s", strings.Join(diff, "\n"))
			}
			for zone, want := range serials {
				if got := soaSerial(t, spec.Ports.DNS, zone); got != want {
					t.Errorf("%s serial %d after the restart, %d before (unchanged content)", zone, got, want)
				}
			}
			if got := d.api(d.personal).principal(t); got != d.principals["personal"] {
				t.Errorf("personal token after the restart: %s", got)
			}
			if got := minTTL(t); got != "9s" {
				t.Errorf("cache.minTTL = %q after the restart; 9s was saved after the upgrade", got)
			}
			n.Stop(20 * time.Second)
			if got, want := schemaVersions(t, spec.DataDir), currentSchemas(); !maps.Equal(got, want) {
				t.Errorf("schema versions after the restart: %v, want %v", got, want)
			}
			if got := settingsObject(t); got != settingsCreated {
				t.Errorf("Kudurru/default after the restart: %s, was %s", got, settingsCreated)
			}
		})
	}

	// The audit trail needs a license key, which only a licensetest build
	// trusts: the same source as the binary under test plus one trusted test
	// key (licensedNodeFixture, D373). It reads the rows 0.1.0 wrote (hash
	// format 1) and the rows this release wrote on top (format 2) as one
	// chain.
	t.Run("AuditTrailAcrossTheUpgrade", func(t *testing.T) {
		licensed, fixtureEnv := licensedNodeFixture(t)
		env := append([]string{}, d.env...)
		for _, kv := range fixtureEnv {
			if strings.HasPrefix(kv, "KUDURRU_LICENSE_KEY_FILE=") {
				env = append(env, kv)
			}
		}
		n := kudurrutest.StartReady(t, licensed, spec, kudurrutest.Options{Env: env})
		defer n.Stop(20 * time.Second)
		var verify struct {
			Verified bool `json:"verified"`
		}
		b := api.must(t, http.MethodGet, "/audit/verify", nil, http.StatusOK)
		if err := json.Unmarshal(b, &verify); err != nil || !verify.Verified {
			t.Fatalf("GET /audit/verify: %s %v", b, err)
		}
		var page struct {
			Items []struct {
				ID       json.Number `json:"id"`
				Action   string      `json:"action"`
				Resource struct {
					Name string `json:"name"`
				} `json:"resource"`
			} `json:"items"`
		}
		b = api.must(t, http.MethodGet, "/audit?order=asc&limit=1000", nil, http.StatusOK)
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		before := 0
		for _, it := range page.Items {
			id, err := it.ID.Int64()
			if err != nil {
				t.Fatalf("audit id %q: %v", it.ID, err)
			}
			if id <= int64(d.auditRows) {
				before++
			}
			seen[it.Action+" "+it.Resource.Name] = true
		}
		if before != d.auditRows {
			t.Errorf("GET /audit lists %d of the %d rows %s wrote", before, d.auditRows, kudurrutest.PreviousRelease)
		}
		for _, want := range []string{"dnszone.create api-test", "blocklist.create ads", "dnszone.create new-test"} {
			if !seen[want] {
				t.Errorf("GET /audit lacks %q", want)
			}
		}
		out, err := exec.Command(licensed, "audit", "verify", "--server", "http://"+spec.Ports.API, "--token", d.breakGlass).CombinedOutput() //nolint:gosec // the test's binary
		if err != nil || !strings.Contains(string(out), "true") {
			t.Errorf("kudurru audit verify: %v\n%s", err, out)
		}
	})

	// Going back is refused: the 0.1.0 binary does not open the upgraded
	// directory, says why, and leaves every database as it is.
	t.Run("PreviousReleaseRefusesTheUpgradedDirectory", func(t *testing.T) {
		upgraded := schemaVersions(t, spec.DataDir)
		n := kudurrutest.Start(t, d.previous, spec, kudurrutest.Options{Env: d.env, Args: []string{"serve", "--config", spec.Config}})
		code, err := n.Wait(30 * time.Second)
		if err != nil {
			t.Fatalf("%s kept running on the upgraded directory: %v\n%s", kudurrutest.PreviousRelease, err, n.Log())
		}
		log := n.Log()
		if code == 0 || !strings.Contains(log, "state.db was written by a newer kudurru") ||
			!strings.Contains(log, fmt.Sprintf("schema version %d", store.SchemaVersion)) {
			t.Errorf("%s on the upgraded directory: exit %d\n%s", kudurrutest.PreviousRelease, code, log)
		}
		if got := schemaVersions(t, spec.DataDir); !maps.Equal(got, upgraded) {
			t.Errorf("the refused start changed the schema versions: %v, were %v", got, upgraded)
		}
		// The directory still serves under this release.
		again := kudurrutest.StartReady(t, kudurruBin, spec, d.options())
		if r := ask(t, spec.Ports.DNS, "host.file.test.", dns.TypeA, false); firstA(r) != "192.0.2.1" {
			t.Errorf("after the refused downgrade: %v", r)
		}
		again.Stop(20 * time.Second)
	})
}

// TestUpgradeFromV01RefusesLegacyObjects: 0.1.0 stored objects that this
// release cannot serve (secondary and stub zones without their
// configuration, an external zone, a second DNSZone for one zone name, a
// free-form policy rewrite). The binary under test refuses to upgrade such
// a directory, names every object and changes nothing, so the 0.1.0 binary
// still runs on it; after the objects are corrected there, the upgrade goes
// through.
func TestUpgradeFromV01RefusesLegacyObjects(t *testing.T) {
	kudurrutest.RequireE2E(t)
	d, old := writeV01Directory(t, "legacy", true)
	d.stop(t, old)
	d.writeFixture(t, "state-legacy.db.gz")
	spec := d.spec
	legacyZones := []string{"legacy-secondary", "legacy-stub", "legacy-external", "legacy-duplicate", "legacy-grant-outside", "legacy-grant-type"}

	refused := kudurrutest.Start(t, kudurruBin, spec, d.options())
	code, err := refused.Wait(60 * time.Second)
	if err != nil {
		t.Fatalf("the binary under test kept running on the legacy directory: %v\n%s", err, refused.Log())
	}
	log := refused.Log()
	if code == 0 {
		t.Fatalf("exit 0 on the legacy directory:\n%s", log)
	}
	for _, want := range []string{
		"error: state.db was written by an earlier kudurru",
		fmt.Sprintf("state.db (schema 1) cannot be upgraded to schema %d: 8 stored objects are not valid in this release", store.SchemaVersion),
		"DNSZone legacy-secondary: spec: Invalid value: secondary zones require secondary configuration",
		"DNSZone legacy-stub: spec: Invalid value: stub zones require nameserver configuration",
		`DNSZone legacy-external: spec.type: Unsupported value: "external"`,
		`DNSZone legacy-duplicate: spec.zoneName: Duplicate value: "API.test.": the zone is also configured by DNSZone "api-test"`,
		`DNSZone api-test: spec.zoneName: Duplicate value: "api.test.": the zone is also configured by DNSZone "legacy-duplicate"`,
		"Policy default/default: spec.rewrites[0]: Invalid value: provide either a CNAME or targets, not both",
		`DNSZone legacy-grant-outside: spec.policy.updatePolicies[0].name: Invalid value: "host.elsewhere.test."`,
		`DNSZone legacy-grant-type: spec.policy.updatePolicies[0].types: Invalid value: "BOGUS"`,
		"state.db was not changed",
		"Start that earlier release on this data directory, correct or delete the listed objects",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, log)
		}
	}
	if got := schemaVersions(t, spec.DataDir); !maps.Equal(got, v01Schemas) {
		t.Fatalf("the refused upgrade changed the schema versions: %v, want %v", got, v01Schemas)
	}
	if got := stateRows(t, spec.DataDir, `SELECT count(*) FROM audit`); got != d.auditRows {
		t.Errorf("the refused upgrade changed the audit trail: %d rows, were %d", got, d.auditRows)
	}

	// The way out the message names: the previous release still runs on the
	// directory, and the operator corrects the objects there.
	previous := kudurrutest.StartReady(t, d.previous, spec, kudurrutest.Options{Env: d.env, Args: []string{"serve", "--config", spec.Config}})
	if strings.Contains(previous.Log(), "first boot") {
		t.Fatalf("%s started over on the refused directory:\n%s", kudurrutest.PreviousRelease, previous.Log())
	}
	if r := ask(t, spec.Ports.DNS, "host.file.test.", dns.TypeA, false); firstA(r) != "192.0.2.1" {
		t.Fatalf("%s after the refused upgrade: %v", kudurrutest.PreviousRelease, r)
	}
	api := d.api(d.breakGlass)
	for _, name := range legacyZones {
		if code, b := api.do(t, http.MethodDelete, "/objects/dnszones/"+name, nil); code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("delete %s on %s: %d %s", name, kudurrutest.PreviousRelease, code, b)
		}
	}
	var policy map[string]any
	if err := json.Unmarshal(api.must(t, http.MethodGet, "/objects/policies/default/default", nil, http.StatusOK), &policy); err != nil {
		t.Fatal(err)
	}
	delete(policy, "status")
	delete(policy["spec"].(map[string]any), "rewrites")
	api.must(t, http.MethodPut, "/objects/policies/default/default", policy, http.StatusOK)
	if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool {
		r, err := exchange(spec.Ports.DNS, "www.api.test.", dns.TypeA, false)
		return err == nil && firstA(r) == "192.0.2.10"
	}) {
		t.Fatalf("%s does not serve api.test. after the correction", kudurrutest.PreviousRelease)
	}
	previous.Stop(20 * time.Second)
	if got := schemaVersions(t, spec.DataDir); !maps.Equal(got, v01Schemas) {
		t.Fatalf("schema versions after the correction on %s: %v", kudurrutest.PreviousRelease, got)
	}

	node := kudurrutest.StartReady(t, kudurruBin, spec, d.options())
	settle(t, spec)
	var diff []string
	if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool {
		got := answers(t, spec.Ports.DNS)
		diff = nil
		for q, want := range d.answers {
			if got[q] != want {
				diff = append(diff, fmt.Sprintf("%s: %q, want %q", q, got[q], want))
			}
		}
		return len(diff) == 0
	}) {
		t.Fatalf("answers after the corrected upgrade:\n%s", strings.Join(diff, "\n"))
	}
	if got := api.principal(t); got != d.principals["break-glass"] {
		t.Errorf("break-glass token after the corrected upgrade: %s", got)
	}
	node.Stop(20 * time.Second)
	if got, want := schemaVersions(t, spec.DataDir), currentSchemas(); !maps.Equal(got, want) {
		t.Errorf("schema versions after the corrected upgrade: %v, want %v", got, want)
	}
}

// bumpSchema raises meta.schema_version of one database of a stopped node by
// one, as a newer release would have left it, keeping the column type.
func bumpSchema(t *testing.T, path string, delta int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE meta SET value = CASE typeof(value) WHEN 'text' THEN CAST(CAST(value AS INTEGER) + ? AS TEXT) ELSE value + ? END WHERE key = 'schema_version'`, delta, delta); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// databaseSums is the SHA-256 of each database file and of its write-ahead
// log, when that holds frames, for before and after comparisons of a start
// that must not write. An empty log and the shared-memory index are left
// out: opening a database in WAL mode creates them without changing data.
func databaseSums(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	for _, path := range paths {
		for _, file := range []string{path, path + "-wal"} {
			b, err := os.ReadFile(file) //nolint:gosec // the test's data directory
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if file != path && len(b) == 0 {
				continue
			}
			sum := sha256.Sum256(b)
			sums[filepath.Base(file)] = hex.EncodeToString(sum[:])
		}
	}
	return sums
}

// TestBuiltBinaryRefusesNewerDatabases: the binary under test does not open
// a data directory in which state.db, local.db or querylog.db was written by
// a newer release. It exits non-zero, names the file, leaves every file as
// it is, and serves the directory again once the file is the right one.
func TestBuiltBinaryRefusesNewerDatabases(t *testing.T) {
	kudurrutest.RequireE2E(t)
	spec, cfg := kudurrutest.NewNodeSpec(t, t.TempDir(), "newer")
	cfg["objects"] = zoneObjects(map[string]string{"ns1": "127.0.0.1", "host": "192.0.2.1"})
	cfg["querylog"] = map[string]any{"enabled": true, "sink": "sqlite", "exclude": map[string]any{"sudn": false}}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	env := []string{"KUDURRU_ADMIN_PASSWORD=" + upgradePassword}
	serves := func(t *testing.T) {
		t.Helper()
		n := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env})
		if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool {
			r, err := exchange(spec.Ports.DNS, "host.e2e.test.", dns.TypeA, false)
			return err == nil && firstA(r) == "192.0.2.1"
		}) {
			t.Fatalf("the node does not serve:\n%s", n.Log())
		}
		n.Stop(20 * time.Second)
	}
	serves(t)
	current := schemaVersions(t, spec.DataDir)
	if !maps.Equal(current, currentSchemas()) {
		t.Fatalf("schema versions of the binary under test: %v, want %v", current, currentSchemas())
	}
	for _, file := range []string{"state.db", "local.db", "querylog.db"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(spec.DataDir, file)
			bumpSchema(t, path, 1)
			newer := schemaVersions(t, spec.DataDir)
			if newer[file] == current[file] {
				t.Fatalf("%s: schema version not raised: %v", file, newer)
			}
			var databases []string
			for _, name := range []string{"state.db", "local.db", "querylog.db"} {
				databases = append(databases, filepath.Join(spec.DataDir, name))
			}
			before := databaseSums(t, databases...)
			n := kudurrutest.Start(t, kudurruBin, spec, kudurrutest.Options{Env: env})
			code, err := n.Wait(30 * time.Second)
			if err != nil {
				t.Fatalf("the node kept running on a newer %s: %v\n%s", file, err, n.Log())
			}
			log := n.Log()
			if code == 0 || !strings.Contains(log, "error: "+file+" was written by a newer kudurru") {
				t.Errorf("newer %s: exit %d\n%s", file, code, log)
			}
			if got := schemaVersions(t, spec.DataDir); !maps.Equal(got, newer) {
				t.Errorf("the refused start changed the schema versions: %v, were %v", got, newer)
			}
			if got := databaseSums(t, databases...); !maps.Equal(got, before) {
				t.Errorf("the refused start changed the databases: %v, were %v", got, before)
			}
			bumpSchema(t, path, -1)
			serves(t)
			if got := schemaVersions(t, spec.DataDir); !maps.Equal(got, current) {
				t.Errorf("schema versions after the file was put back: %v, want %v", got, current)
			}
		})
	}
}
