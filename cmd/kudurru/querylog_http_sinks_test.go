package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/sinktest"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// sinkSample reads one sample of a kudurru_sink_* or drop series from the
// registry the metrics listener serves.
func sinkSample(t *testing.T, reg *prometheus.Registry, family, sink string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			want := map[string]string{"sink": sink}
			if family == "kudurru_querylog_dropped_total" {
				want["stage"] = querylog.DropStageSink
			}
			for _, label := range m.GetLabel() {
				if expected, checked := want[label.GetName()]; checked {
					if label.GetValue() != expected {
						continue metrics
					}
					delete(want, label.GetName())
				}
			}
			if len(want) != 0 {
				continue
			}
			if counter := m.GetCounter(); counter != nil {
				return counter.GetValue(), true
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// writeSecret replaces a credential file the way a mounted Secret is
// updated: a rename into place.
func writeSecret(t *testing.T, path, value string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

// TestQuerylogRuntimeHTTPSinksReadOnlyGrantedCredentials runs the ClickHouse
// and Loki sinks through their production factories and the production
// credential resolver, on the real runtime (collector, tail, querylog.db).
// A reference is honored only when the node's own configuration grants the
// exact file; a sink whose reference is not granted contacts nothing; a
// rotated file is picked up when the destination refuses the old value; and
// what the sinks send is what the privacy mode keeps.
func TestQuerylogRuntimeHTTPSinksReadOnlyGrantedCredentials(t *testing.T) {
	secrets := t.TempDir()
	passwordFile, tokenFile, otherFile := filepath.Join(secrets, "clickhouse-password"), filepath.Join(secrets, "loki-token"), filepath.Join(secrets, "not-granted")
	writeSecret(t, passwordFile, "first-password\n")
	writeSecret(t, tokenFile, "first-token\n")
	writeSecret(t, otherFile, "a-file-the-operator-did-not-delegate\n")

	warehouse := sinktest.New(t, http.StatusOK, false)
	logs := sinktest.New(t, http.StatusNoContent, false)
	denied := sinktest.New(t, http.StatusOK, false)
	var mu sync.Mutex
	password := "first-password"
	warehouse.Answer(func(_ int, r sinktest.Request) sinktest.Answer {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != sinktest.Basic("kudurru", password) {
			return sinktest.Answer{Status: http.StatusForbidden, Header: http.Header{"X-ClickHouse-Exception-Code": {"516"}}}
		}
		return sinktest.Answer{}
	})

	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Node.Name = "node-a"
	cfg.Querylog.Exclude.SUDN = false
	cfg.Querylog.Exclude.Domains = []string{"*.excluded.example"}
	// The node's operator delegated exactly two files.
	cfg.Credentials.AllowedFiles = []string{passwordFile, tokenFile}
	cfg.Querylog.Sinks = []config.QuerylogSink{
		{Name: "warehouse", Type: config.QuerylogSinkClickHouse, BatchSize: 1, Attempts: 2, RetryBackoff: config.Duration(100 * time.Millisecond), ClickHouse: &config.QuerylogClickHouse{
			URL: warehouse.URL, Username: "kudurru", PasswordRef: &config.CredentialRef{File: passwordFile}, Compression: config.QuerylogCompressionNone}},
		{Name: "logs", Type: config.QuerylogSinkLoki, BatchSize: 1, Loki: &config.QuerylogLoki{
			URL: logs.URL, Tenant: "home", BearerTokenRef: &config.CredentialRef{File: tokenFile}, Compression: config.QuerylogCompressionNone}},
		{Name: "denied", Type: config.QuerylogSinkClickHouse, BatchSize: 1, RetryBackoff: config.Duration(100 * time.Millisecond), ClickHouse: &config.QuerylogClickHouse{
			URL: denied.URL, Username: "kudurru", PasswordRef: &config.CredentialRef{File: otherFile}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logOutput := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := prometheus.NewRegistry()
	r, err := newQuerylogRuntime(t.Context(), cfg, reg, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = r.close(context.Background())
		}
	})
	if len(warehouse.Requests())+len(logs.Requests())+len(denied.Requests()) != 0 {
		t.Fatal("preparing the sinks contacted a destination")
	}
	enqueue := func(name string) {
		t.Helper()
		ev := &querylog.Event{Time: time.Now(), Node: cfg.Node.Name, Name: name, Client: netip.MustParseAddr("192.0.2.9"), ClientName: "laptop", QType: dns.TypeA, QClass: dns.ClassINET, Answer: "192.0.2.80"}
		if !r.async.Enqueue(ev) {
			t.Fatal("collector refused", name)
		}
	}
	wait := func(what string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !fn() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s\n%s", what, logOutput.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	sent := func(sink string) float64 { v, _ := sinkSample(t, reg, "kudurru_sink_sent_total", sink); return v }
	rows := func() int64 {
		t.Helper()
		page, err := r.reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return page.RecordsTotal
	}

	enqueue("one.example.")
	enqueue("never.excluded.example.")
	wait("both granted sinks to deliver the first query", func() bool { return sent("warehouse") == 1 && sent("logs") == 1 })
	wait("the local row", func() bool { return rows() == 1 })
	row := sinktest.ClickHouseRows(t, warehouse.Requests()[0])[0]
	if row["domain"] != "one.example" || row["client"] != "192.0.2.9" || row["node"] != "node-a" || row["client_name"] != "laptop" || row["answer"] != "192.0.2.80" {
		t.Fatalf("ClickHouse row under privacy full: %v", row)
	}
	push := logs.Requests()[0]
	if push.Header.Get("Authorization") != "Bearer first-token" || push.Header.Get("X-Scope-OrgID") != "home" {
		t.Fatal("the Loki push did not present the granted token and the tenant")
	}
	stream := sinktest.LokiStreams(t, push)[0]
	if !reflect.DeepEqual(stream.Labels, map[string]string{"job": "kudurru", "node": "node-a", "status_class": "permitted"}) || stream.Entries[0].Metadata["domain"] != "one.example" {
		t.Fatalf("Loki stream: %+v", stream)
	}
	// The row and the line carry the ID the local query log gave the query.
	page, err := r.reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if id := page.Queries[0].ID; id != 1 || row["id"] != json.Number("1") || stream.Entries[0].LineJSON(t)["id"] != json.Number("1") {
		t.Fatalf("IDs: local %d, ClickHouse %v, Loki %v", id, row["id"], stream.Entries[0].LineJSON(t)["id"])
	}

	// The sink whose reference is not granted never opened: it sent
	// nothing, its attempts are counted, and the log says why.
	wait("the refused reference to be counted", func() bool {
		errorsSeen, _ := sinkSample(t, reg, "kudurru_sink_errors_total", "denied")
		return errorsSeen >= 2
	})
	if len(denied.Requests()) != 0 || sent("denied") != 0 {
		t.Fatal("a sink sent a batch without the credential its settings name")
	}
	if queued, _ := sinkSample(t, reg, "kudurru_sink_queue_length", "denied"); queued != 1 {
		t.Fatalf("the refused sink holds %v queries, want the one it could not send", queued)
	}
	var why []map[string]any
	for _, rec := range logRecords(t, logOutput, "query log sink open failed") {
		if rec["sink"] == "denied" {
			why = append(why, rec)
		}
	}
	if len(why) == 0 || !strings.Contains(why[0]["err"].(string), "ClickHouse: password: credential reference is not allowed") {
		t.Fatalf("the log does not explain the refused reference: %v", why)
	}

	// Rotation: the destination stops accepting the old password, the file
	// is replaced, and the refused batch goes out again with the new value.
	mu.Lock()
	password = "second-password"
	mu.Unlock()
	writeSecret(t, passwordFile, "second-password\n")
	enqueue("two.example.")
	wait("the batch after the rotation", func() bool { return sent("warehouse") == 2 })
	requests := warehouse.Requests()
	if len(requests) != 3 || requests[1].Header.Get("Authorization") != sinktest.Basic("kudurru", "first-password") || requests[2].Header.Get("Authorization") != sinktest.Basic("kudurru", "second-password") {
		t.Fatalf("%d requests to ClickHouse; the refused batch was not repeated with the rotated password", len(requests))
	}
	if string(requests[1].Body) != string(requests[2].Body) {
		t.Fatal("the repeated batch differs from the refused one")
	}
	if dropped, _ := sinkSample(t, reg, "kudurru_querylog_dropped_total", "warehouse"); dropped != 0 {
		t.Fatalf("a rotation cost %v queries", dropped)
	}
	if errorsSeen, _ := sinkSample(t, reg, "kudurru_sink_errors_total", "warehouse"); errorsSeen != 1 {
		t.Fatalf("kudurru_sink_errors_total{sink=warehouse} = %v, want the one refused attempt", errorsSeen)
	}

	// A privacy change keeps the entries and their sinks; from then on the
	// destinations receive only what the new mode keeps.
	next := cfg.Querylog
	next.Privacy = "hide_domains_clients"
	if err := r.reload(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	enqueue("three.example.")
	wait("the query after the privacy change", func() bool { return sent("warehouse") == 3 && sent("logs") == 3 })
	hidden := sinktest.ClickHouseRows(t, warehouse.Requests()[3])[0]
	if hidden["domain"] != querylog.HiddenName || hidden["client"] != "0.0.0.0" || hidden["client_name"] != nil || hidden["answer"] != nil {
		t.Fatalf("ClickHouse row under hide_domains_clients: %v", hidden)
	}
	last := logs.Requests()[2]
	if body := string(last.Body); strings.Contains(body, "three.example") || strings.Contains(body, "192.0.2.9") || strings.Contains(body, "laptop") {
		t.Fatalf("the Loki push carries hidden values: %s", body)
	}
	if meta := sinktest.LokiStreams(t, last)[0].Entries[0].Metadata; meta["domain"] != querylog.HiddenName || meta["client"] != "0.0.0.0" {
		t.Fatalf("Loki structured metadata under hide_domains_clients: %v", meta)
	}
	for _, r := range append(warehouse.Requests(), logs.Requests()...) {
		if strings.Contains(string(r.Body), "excluded.example") {
			t.Fatal("an excluded name was exported")
		}
	}

	// A clean close drains the healthy sinks. The sink that never opened
	// has nothing it could send and does not hold the shutdown up.
	closeCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := r.close(closeCtx); err != nil {
		t.Fatalf("close: %v", err)
	}
	closed = true
	if got := len(warehouse.Requests()) + len(logs.Requests()); got != 7 {
		t.Fatalf("%d requests in total, want 4 to ClickHouse and 3 to Loki", got)
	}
	if len(denied.Requests()) != 0 {
		t.Fatal("the refused sink sent a batch at shutdown")
	}
	out := logOutput.String()
	for _, secret := range []string{"first-password", "second-password", "first-token", "a-file-the-operator-did-not-delegate", "Zmlyc3QtcGFzc3dvcmQ", "c2Vjb25kLXBhc3N3b3Jk"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the server log repeats a credential (%s)", secret)
		}
	}
	for _, name := range []string{"one.example", "two.example", "three.example"} {
		if strings.Contains(out, name) {
			t.Fatalf("the server log repeats a query name (%s):\n%s", name, out)
		}
	}
}

// TestQuerylogSinkCredentialsHonorOnlyTheNodeGrants pins the resolver the
// runtime hands to the sinks: the three reference forms, each bounded by
// what this node delegated.
func TestQuerylogSinkCredentialsHonorOnlyTheNodeGrants(t *testing.T) {
	dir := t.TempDir()
	granted, other, gone := filepath.Join(dir, "granted"), filepath.Join(dir, "other"), filepath.Join(dir, "gone")
	writeSecret(t, granted, "from-file")
	writeSecret(t, other, "must-not-be-read")
	t.Setenv("KUDURRU_TEST_SINK_GRANTED", "from-env")
	t.Setenv("KUDURRU_TEST_SINK_OTHER", "must-not-be-read")
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Credentials.AllowedFiles = []string{granted, gone}
	cfg.Credentials.AllowedEnvironment = []string{"KUDURRU_TEST_SINK_GRANTED", "KUDURRU_TEST_SINK_UNSET"}

	var asked []string
	secrets := &querylogSecrets{Namespace: "kudurru", Read: func(_ context.Context, name, key string) ([]byte, error) {
		asked = append(asked, name+"/"+key)
		if name == "export" && key == "password" {
			return []byte("from-secret"), nil
		}
		return nil, errors.New(`secrets "` + name + `" not found`)
	}}
	withSecrets, err := querylogSinkCredentials(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	without, err := querylogSinkCredentials(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	secret := func(name, key string) config.CredentialRef {
		return config.CredentialRef{SecretRef: &config.CredentialSecretKey{Name: name, Key: key}}
	}
	for name, tc := range map[string]struct {
		resolve func(context.Context, config.CredentialRef) ([]byte, error)
		ref     config.CredentialRef
		want    string
		wantErr string
	}{
		"granted file":              {without, config.CredentialRef{File: granted}, "from-file", ""},
		"file that is not granted":  {without, config.CredentialRef{File: other}, "", "credential reference is not allowed"},
		"path beside a grant":       {without, config.CredentialRef{File: granted + "/../other"}, "", "credential reference is not allowed"},
		"granted file that is gone": {without, config.CredentialRef{File: gone}, "", "credential is unavailable"},
		"granted variable":          {without, config.CredentialRef{Env: "KUDURRU_TEST_SINK_GRANTED"}, "from-env", ""},
		"granted variable, unset":   {without, config.CredentialRef{Env: "KUDURRU_TEST_SINK_UNSET"}, "", "credential is unavailable"},
		"variable not granted":      {without, config.CredentialRef{Env: "KUDURRU_TEST_SINK_OTHER"}, "", "credential reference is not allowed"},
		"secret without a cluster":  {without, secret("export", "password"), "", "credential reference is not allowed"},
		"secret":                    {withSecrets, secret("export", "password"), "from-secret", ""},
		"missing secret":            {withSecrets, secret("absent", "password"), "", "credential is unavailable"},
		"no form":                   {withSecrets, config.CredentialRef{}, "", "credential reference is not allowed"},
		"two forms":                 {withSecrets, config.CredentialRef{File: granted, Env: "KUDURRU_TEST_SINK_GRANTED"}, "", "credential reference is not allowed"},
	} {
		value, err := tc.resolve(t.Context(), tc.ref)
		switch {
		case tc.wantErr == "" && (err != nil || string(value) != tc.want):
			t.Errorf("%s: %q, %v", name, value, err)
		case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr || len(value) != 0):
			t.Errorf("%s: %q, %v, want %q", name, value, err, tc.wantErr)
		}
		if err != nil && (strings.Contains(err.Error(), "must-not-be-read") || strings.Contains(err.Error(), "absent")) {
			t.Errorf("%s: the error repeats a value or the reference: %v", name, err)
		}
	}
	if !reflect.DeepEqual(asked, []string{"export/password", "absent/password"}) {
		t.Fatalf("Secrets read: %v", asked)
	}
	// Each call reads the source again, which is what a rotation relies on.
	writeSecret(t, granted, "rotated")
	if value, err := without(t.Context(), config.CredentialRef{File: granted}); err != nil || string(value) != "rotated" {
		t.Fatalf("a replaced file was not read again: %q, %v", value, err)
	}
	if _, err := querylogSinkCredentials(cfg, &querylogSecrets{Namespace: "kudurru"}); err == nil {
		t.Fatal("a Secret reader without a read function was accepted")
	}
	if _, err := querylogSinkCredentials(cfg, &querylogSecrets{Namespace: "Not A Namespace", Read: secrets.Read}); err == nil {
		t.Fatal("a Secret reader without a valid namespace was accepted")
	}
}

// TestQuerylogSinkSecretsNeedTheLabel: on Kubernetes the server can read
// every Secret of its namespace. The sinks get only those that carry the
// label, in that namespace, through the production reader.
func TestQuerylogSinkSecretsNeedTheLabel(t *testing.T) {
	secret := func(namespace, name string, labels map[string]string, data map[string]string) *corev1.Secret {
		out := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels}, Data: map[string][]byte{}}
		for k, v := range data {
			out.Data[k] = []byte(v)
		}
		return out
	}
	marked := map[string]string{querylogSinkSecretLabel: "true"}
	cluster := fake.NewClientset(
		secret("kudurru", "export", marked, map[string]string{"password": "from-the-marked-secret"}),
		secret("kudurru", "server-keys", nil, map[string]string{"encryption-key": "must-not-leave"}),
		secret("kudurru", "half-marked", map[string]string{querylogSinkSecretLabel: "false"}, map[string]string{"token": "must-not-leave"}),
		secret("other", "export", marked, map[string]string{"password": "of-another-namespace"}),
		secret("other", "only-there", marked, map[string]string{"password": "of-another-namespace"}),
	)
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := querylogSinkCredentials(cfg, querylogSecretsFrom(cluster.CoreV1().Secrets("kudurru"), "kudurru"))
	if err != nil {
		t.Fatal(err)
	}
	ref := func(name, key string) config.CredentialRef {
		return config.CredentialRef{SecretRef: &config.CredentialSecretKey{Name: name, Key: key}}
	}
	if value, err := resolve(t.Context(), ref("export", "password")); err != nil || string(value) != "from-the-marked-secret" {
		t.Fatalf("a marked Secret of the server's namespace: %q, %v", value, err)
	}
	const refused = `credential reference is not allowed: the Secret needs the label kudurru.io/query-log-sink: "true"`
	for name, tc := range map[string]struct {
		ref  config.CredentialRef
		want string
	}{
		"a Secret without the label":       {ref("server-keys", "encryption-key"), refused},
		"a label with another value":       {ref("half-marked", "token"), refused},
		"a Secret of another namespace":    {ref("only-there", "password"), "credential is unavailable"},
		"a Secret that does not exist":     {ref("absent", "password"), "credential is unavailable"},
		"a key the Secret does not have":   {ref("export", "token"), "credential is unavailable"},
		"a name that is not a Secret's":    {ref("../other/export", "password"), "credential reference is not allowed"},
		"a key that is not a Secret key's": {ref("export", "pass word"), "credential reference is not allowed"},
	} {
		value, err := resolve(t.Context(), tc.ref)
		if err == nil || err.Error() != tc.want || len(value) != 0 {
			t.Errorf("%s: %q, %v, want %q", name, value, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "must-not-leave") {
			t.Errorf("%s: the error repeats the value", name)
		}
	}
	// Rotation: the next read sees the Secret as it is now, and a label
	// that was removed ends the delegation.
	current, err := cluster.CoreV1().Secrets("kudurru").Get(t.Context(), "export", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.Data["password"] = []byte("rotated")
	if _, err := cluster.CoreV1().Secrets("kudurru").Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if value, err := resolve(t.Context(), ref("export", "password")); err != nil || string(value) != "rotated" {
		t.Fatalf("a changed Secret was not read again: %q, %v", value, err)
	}
	current.Labels = nil
	if _, err := cluster.CoreV1().Secrets("kudurru").Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t.Context(), ref("export", "password")); err == nil || err.Error() != refused {
		t.Fatalf("a Secret whose label was removed: %v", err)
	}
}
