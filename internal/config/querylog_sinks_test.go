package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestQuerylogHTTPSinks: the clickhouse and loki settings of querylog.sinks
// (D371) load from the file, keep their defaults unset, survive the flat
// form and the cluster document, and hold references to credentials, never
// values.
func TestQuerylogHTTPSinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
store: { driver: kube }
querylog:
  sinks:
    - name: warehouse
      type: clickhouse
      batchSize: 5000
      flushInterval: 5s
      clickhouse:
        url: https://clickhouse.example:8443
        database: dns
        table: query_log
        username: kudurru
        passwordRef: { file: /run/secrets/clickhouse-password }
        caRef: { secretRef: { name: clickhouse-tls, key: ca.crt } }
        compression: none
    - name: logs
      type: loki
      loki:
        url: http://loki.monitoring:3100
        tenant: home
        bearerTokenRef: { env: LOKI_TOKEN }
        labels: { job: dns, site: home }
        structuredMetadata: false
    - name: minimal
      type: loki
      loki: { url: "https://logs.example/prefix" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	want := []QuerylogSink{
		{Name: "warehouse", Type: QuerylogSinkClickHouse, BatchSize: 5000, FlushInterval: Duration(5e9), ClickHouse: &QuerylogClickHouse{
			URL: "https://clickhouse.example:8443", Database: "dns", Table: "query_log", Username: "kudurru",
			PasswordRef: &CredentialRef{File: "/run/secrets/clickhouse-password"},
			CARef:       &CredentialRef{SecretRef: &CredentialSecretKey{Name: "clickhouse-tls", Key: "ca.crt"}}, Compression: QuerylogCompressionNone,
		}},
		{Name: "logs", Type: QuerylogSinkLoki, Loki: &QuerylogLoki{
			URL: "http://loki.monitoring:3100", Tenant: "home", BearerTokenRef: &CredentialRef{Env: "LOKI_TOKEN"},
			Labels: map[string]string{"job": "dns", "site": "home"}, StructuredMetadata: &off,
		}},
		{Name: "minimal", Type: QuerylogSinkLoki, Loki: &QuerylogLoki{URL: "https://logs.example/prefix"}},
	}
	if !reflect.DeepEqual(cfg.Querylog.Sinks, want) {
		got, _ := json.MarshalIndent(cfg.Querylog.Sinks, "", " ")
		t.Fatalf("sinks:\n%s", got)
	}
	again, err := FromFlat(Flatten(cfg))
	if err != nil || !reflect.DeepEqual(again.Querylog.Sinks, want) {
		t.Fatalf("flat round trip: %v", err)
	}
	// The flat form is what GET /settings shows and the cluster document
	// stores: references only, and unset fields stay unset.
	flat := Flatten(cfg)["querylog.sinks"].([]any)
	if got, want := flat[2], map[string]any{"name": "minimal", "type": "loki", "loki": map[string]any{"url": "https://logs.example/prefix"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unset fields must stay unset in the flat form: %#v", got)
	}
	if got, want := flat[0].(map[string]any)["clickhouse"].(map[string]any)["passwordRef"], map[string]any{"file": "/run/secrets/clickhouse-password"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("passwordRef in the flat form: %#v", got)
	}
	// Defaults are applied by the accessors, not written back.
	minimal, warehouse := cfg.Querylog.Sinks[2].Loki, cfg.Querylog.Sinks[0].ClickHouse
	if !minimal.UsesStructuredMetadata() || cfg.Querylog.Sinks[1].Loki.UsesStructuredMetadata() {
		t.Error("structured metadata defaults to true and follows the setting")
	}
	if got := minimal.StreamLabels(); !reflect.DeepEqual(got, map[string]string{"job": "kudurru"}) {
		t.Errorf("default labels: %v", got)
	}
	if got := cfg.Querylog.Sinks[1].Loki.StreamLabels(); !reflect.DeepEqual(got, map[string]string{"job": "dns", "site": "home"}) {
		t.Errorf("labels: %v", got)
	}
	if warehouse.DatabaseName() != "dns" || warehouse.TableName() != "query_log" || GzipBody(warehouse.Compression) || !GzipBody(minimal.Compression) {
		t.Errorf("clickhouse accessors: %+v", warehouse)
	}
	if d := (QuerylogClickHouse{}); d.DatabaseName() != "kudurru" || d.TableName() != "queries" {
		t.Errorf("clickhouse defaults: %s.%s", d.DatabaseName(), d.TableName())
	}
	next, _, err := ApplyClusterDocument(cfg, map[string]any{"querylog": map[string]any{"sinks": []any{map[string]any{
		"name": "published", "type": "clickhouse", "clickhouse": map[string]any{"url": "http://127.0.0.1:8123", "username": "kudurru", "passwordRef": map[string]any{"secretRef": map[string]any{"name": "ch", "key": "password"}}},
	}}}})
	if err != nil || len(next.Querylog.Sinks) != 1 || next.Querylog.Sinks[0].ClickHouse == nil || next.Querylog.Sinks[0].ClickHouse.PasswordRef.SecretRef.Key != "password" {
		t.Fatalf("cluster document: %v", err)
	}
	if changed := Changed(cfg, next); !reflect.DeepEqual(changed, []string{"querylog.sinks"}) {
		t.Fatalf("changed: %v", changed)
	}
}

func TestQuerylogHTTPSinksValidation(t *testing.T) {
	valid := func(driver string) *Config {
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Store.Driver = driver
		return cfg
	}
	clickhouse := func(edit func(*QuerylogClickHouse)) func(*Config) {
		return func(c *Config) {
			ch := &QuerylogClickHouse{URL: "https://clickhouse.example:8443"}
			edit(ch)
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: QuerylogSinkClickHouse, ClickHouse: ch}}
		}
	}
	loki := func(edit func(*QuerylogLoki)) func(*Config) {
		return func(c *Config) {
			l := &QuerylogLoki{URL: "https://loki.example"}
			edit(l)
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: QuerylogSinkLoki, Loki: l}}
		}
	}
	manyLabels := map[string]string{}
	for i := range MaxQuerylogLokiLabels + 1 {
		manyLabels["l"+string(rune('a'+i))] = "v"
	}
	for name, tc := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"clickhouse without settings": {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: QuerylogSinkClickHouse}} }, "querylog.sinks[0].clickhouse is required for a sink of type clickhouse"},
		"loki without settings":       {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: QuerylogSinkLoki}} }, "querylog.sinks[0].loki is required for a sink of type loki"},
		"settings of another type": {func(c *Config) {
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: QuerylogSinkSlog, Loki: &QuerylogLoki{URL: "https://loki.example"}}}
		}, "querylog.sinks[0].loki: these settings belong to a sink of type loki, not slog"},
		"both settings": {func(c *Config) {
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: QuerylogSinkLoki, Loki: &QuerylogLoki{URL: "https://loki.example"}, ClickHouse: &QuerylogClickHouse{URL: "https://c.example"}}}
		}, "querylog.sinks[0].clickhouse: these settings belong to a sink of type clickhouse, not loki"},
		"missing url":                           {clickhouse(func(c *QuerylogClickHouse) { c.URL = "" }), "querylog.sinks[0].clickhouse.url is required"},
		"url scheme":                            {clickhouse(func(c *QuerylogClickHouse) { c.URL = "tcp://clickhouse:9000" }), "querylog.sinks[0].clickhouse.url is not an http or https URL"},
		"url with password":                     {clickhouse(func(c *QuerylogClickHouse) { c.URL = "https://kudurru:hunter2@clickhouse.example" }), "querylog.sinks[0].clickhouse.url must not carry a username or password; use username and passwordRef"},
		"url with query":                        {clickhouse(func(c *QuerylogClickHouse) { c.URL = "https://clickhouse.example/?password=hunter2" }), "querylog.sinks[0].clickhouse.url must not carry a query or fragment"},
		"url of another scheme with a password": {clickhouse(func(c *QuerylogClickHouse) { c.URL = "ftp://kudurru:hunter2@clickhouse.example" }), "querylog.sinks[0].clickhouse.url is not an http or https URL"},
		"url with dots":                         {loki(func(l *QuerylogLoki) { l.URL = "https://loki.example/a/../b" }), "must not contain . or .. path segments"},
		"url with space":                        {loki(func(l *QuerylogLoki) { l.URL = "https://loki.example/a b" }), "is not an http or https URL of at most 2048 characters without spaces"},
		"url port":                              {loki(func(l *QuerylogLoki) { l.URL = "https://loki.example:0" }), "has an invalid port"},
		"database":                              {clickhouse(func(c *QuerylogClickHouse) { c.Database = "dns`; DROP" }), "querylog.sinks[0].clickhouse.database:"},
		"table":                                 {clickhouse(func(c *QuerylogClickHouse) { c.Table = "query-log" }), `querylog.sinks[0].clickhouse.table: "query-log" is not a name of letters, digits and underscores`},
		"password without user": {clickhouse(func(c *QuerylogClickHouse) { c.PasswordRef = &CredentialRef{File: "/run/secrets/p"} }),
			"querylog.sinks[0].clickhouse.username is required with passwordRef"},
		"username with colon": {clickhouse(func(c *QuerylogClickHouse) { c.Username = "a:b" }), "querylog.sinks[0].clickhouse.username: use at most 128 characters without a colon"},
		"compression":         {clickhouse(func(c *QuerylogClickHouse) { c.Compression = "zstd" }), `querylog.sinks[0].clickhouse.compression: unknown value "zstd" (gzip, none)`},
		"empty reference": {clickhouse(func(c *QuerylogClickHouse) { c.Username, c.PasswordRef = "u", &CredentialRef{} }),
			"querylog.sinks[0].clickhouse.passwordRef: set exactly one of secretRef, file or env"},
		"two references": {clickhouse(func(c *QuerylogClickHouse) {
			c.Username, c.PasswordRef = "u", &CredentialRef{File: "/run/secrets/p", Env: "P"}
		}), "querylog.sinks[0].clickhouse.passwordRef: set exactly one of secretRef, file or env"},
		"relative file": {clickhouse(func(c *QuerylogClickHouse) { c.Username, c.PasswordRef = "u", &CredentialRef{File: "secrets/p"} }),
			"querylog.sinks[0].clickhouse.passwordRef.file: use a clean absolute path"},
		"unclean file": {clickhouse(func(c *QuerylogClickHouse) { c.CARef = &CredentialRef{File: "/run/secrets/../ca.pem"} }),
			"querylog.sinks[0].clickhouse.caRef.file: use a clean absolute path"},
		"environment name": {loki(func(l *QuerylogLoki) { l.BearerTokenRef = &CredentialRef{Env: "LOKI-TOKEN"} }),
			`querylog.sinks[0].loki.bearerTokenRef.env: "LOKI-TOKEN" is not the name of an environment variable`},
		"tenant": {loki(func(l *QuerylogLoki) { l.Tenant = "a|b" }), `querylog.sinks[0].loki.tenant: "a|b" is not a Loki tenant ID`},
		"basic and bearer": {loki(func(l *QuerylogLoki) {
			l.Username, l.PasswordRef, l.BearerTokenRef = "u", &CredentialRef{Env: "P"}, &CredentialRef{Env: "T"}
		}), "querylog.sinks[0].loki: use either username with passwordRef or bearerTokenRef, not both"},
		"user without password": {loki(func(l *QuerylogLoki) { l.Username = "u" }), "querylog.sinks[0].loki.passwordRef is required with username"},
		"password without a user": {loki(func(l *QuerylogLoki) { l.PasswordRef = &CredentialRef{Env: "P"} }),
			"querylog.sinks[0].loki.username is required with passwordRef"},
		"label name":        {loki(func(l *QuerylogLoki) { l.Labels = map[string]string{"site-name": "home"} }), `querylog.sinks[0].loki.labels: "site-name" is not a label name`},
		"reserved prefix":   {loki(func(l *QuerylogLoki) { l.Labels = map[string]string{"__name__": "x"} }), `querylog.sinks[0].loki.labels: "__name__" is not a label name`},
		"label of the sink": {loki(func(l *QuerylogLoki) { l.Labels = map[string]string{"node": "x"} }), `querylog.sinks[0].loki.labels: "node" is set by the sink for every query`},
		"status class":      {loki(func(l *QuerylogLoki) { l.Labels = map[string]string{"status_class": "x"} }), `"status_class" is set by the sink`},
		"empty label":       {loki(func(l *QuerylogLoki) { l.Labels = map[string]string{"site": ""} }), "querylog.sinks[0].loki.labels[site]: use a value of 1 to 128 characters"},
		"too many labels":   {loki(func(l *QuerylogLoki) { l.Labels = manyLabels }), "querylog.sinks[0].loki.labels: 17 labels, at most 16 are supported"},
	} {
		cfg := valid("kube")
		tc.edit(cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		// A refused URL is where a password gets typed by mistake, and the
		// message of a refused settings change is logged and audited.
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the message repeats the URL: %v", name, err)
		}
	}

	// A Secret reference needs the Kubernetes API: it is refused where no
	// node could ever read it, and accepted on the Kubernetes driver.
	secret := clickhouse(func(c *QuerylogClickHouse) {
		c.Username, c.PasswordRef = "kudurru", &CredentialRef{SecretRef: &CredentialSecretKey{Name: "query-log-export", Key: "password"}}
	})
	cfg := valid("sqlite")
	secret(cfg)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "querylog.sinks[0].clickhouse.passwordRef.secretRef: Secrets are read on Kubernetes only") {
		t.Errorf("secret reference on SQLite: %v", err)
	}
	cfg = valid("kube")
	secret(cfg)
	if err := cfg.Validate(); err != nil {
		t.Errorf("secret reference on Kubernetes: %v", err)
	}
	for name, ref := range map[string]*CredentialSecretKey{"secret name": {Name: "Query_Log", Key: "password"}, "secret key": {Name: "export", Key: "pass word"}, "no key": {Name: "export"}} {
		cfg = valid("kube")
		loki(func(l *QuerylogLoki) { l.BearerTokenRef = &CredentialRef{SecretRef: ref} })(cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "querylog.sinks[0].loki.bearerTokenRef.secretRef.") {
			t.Errorf("%s: %v", name, err)
		}
	}

	// File and environment references are admitted on every driver: whether
	// a node may read them is that node's credentials.allowed* decision.
	cfg = valid("sqlite")
	cfg.Querylog.Sinks = []QuerylogSink{
		{Name: "warehouse", Type: QuerylogSinkClickHouse, ClickHouse: &QuerylogClickHouse{URL: "http://127.0.0.1:8123/", Username: "kudurru", PasswordRef: &CredentialRef{File: "/run/secrets/clickhouse"}, Compression: QuerylogCompressionGzip}},
		{Name: "logs", Type: QuerylogSinkLoki, Loki: &QuerylogLoki{URL: "https://logs.example/loki/api/v1/push", Tenant: "team-a_1", Username: "1234", PasswordRef: &CredentialRef{Env: "LOKI_PASSWORD"}, Labels: map[string]string{"job": "kudurru", "_site": "home lab"}}},
		{Name: "open", Type: QuerylogSinkClickHouse, ClickHouse: &QuerylogClickHouse{URL: "http://[::1]:8123", Username: "default"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid HTTP sinks refused: %v", err)
	}
}
