package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQuerylogDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Querylog{
		Enabled:   true,
		Sink:      "sqlite",
		Privacy:   "full",
		Anonymize: "prefix",
		Retention: QuerylogRetention{Raw: Duration(720 * time.Hour), MaxDBBytes: 10 << 30},
		Exclude:   QuerylogExclude{Domains: []string{}, Clients: []string{}, SUDN: true},
		Sinks:     []QuerylogSink{},
	}
	if !reflect.DeepEqual(cfg.Querylog, want) {
		t.Errorf("querylog defaults (D59):\n got %+v\nwant %+v", cfg.Querylog, want)
	}
}

func TestQuerylogFileAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
querylog: { enabled: true, privacy: anonymize_client, anonymize: pseudonym, retention: { raw: 48h, maxDBBytes: 2GB },
            exclude: { domains: ["*.lan", "/^ads\\./"], clients: ["10.0.0.0/8", "printer"], sudn: false } }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, []string{"KUDURRU_QUERYLOG__RETENTION__MAXDBBYTES=512MiB", "KUDURRU_QUERYLOG__SINK=slog"})
	if err != nil {
		t.Fatal(err)
	}
	q := cfg.Querylog
	if q.Privacy != "anonymize_client" || q.Anonymize != "pseudonym" || q.Sink != "slog" || q.Retention.Raw.Std() != 48*time.Hour {
		t.Errorf("querylog file/env values: %+v", q)
	}
	if q.Retention.MaxDBBytes != 512<<20 {
		t.Errorf("maxDBBytes env override: %d", q.Retention.MaxDBBytes)
	}
	if !reflect.DeepEqual(q.Exclude, QuerylogExclude{Domains: []string{"*.lan", `/^ads\./`}, Clients: []string{"10.0.0.0/8", "printer"}}) {
		t.Errorf("exclude: %+v", q.Exclude)
	}
}

func TestQuerylogValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte(`
querylog: { sink: kafka, privacy: some, anonymize: blur, retention: { raw: -1h, maxDBBytes: -5 }, exclude: { domains: ["/([/"] } }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("invalid querylog settings must be rejected")
	}
	for _, want := range []string{
		`querylog.sink: unknown sink "kafka"`,
		`querylog.privacy: unknown mode "some"`,
		`querylog.anonymize: unknown method "blur"`,
		"querylog.retention.raw: -1h0m0s is negative",
		"querylog.retention.maxDBBytes: -5 is negative",
		"querylog.exclude.domains",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// TestQuerylogSinks: querylog.sinks (D371) loads from the file, keeps unset
// limits as zero (the defaults), and survives the flat form the API, the
// settings file and the cluster document use.
func TestQuerylogSinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`
querylog:
  sinks:
    - { name: log-lines, type: slog }
    - { name: tuned, type: slog, queueSize: 500, batchSize: 50, flushInterval: 2s, timeout: 30s, attempts: 1, retryBackoff: 500ms }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Two slog sinks are refused; load them one at a time.
	if _, err := Load(path, nil); err == nil || !strings.Contains(err.Error(), "querylog.sinks[1].type: a second slog sink") {
		t.Fatalf("two slog sinks: %v", err)
	}
	if err := os.WriteFile(path, []byte(`
querylog: { sinks: [ { name: tuned, type: slog, queueSize: 500, batchSize: 50, flushInterval: 2s, timeout: 30s, attempts: 1, retryBackoff: 500ms } ] }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []QuerylogSink{{Name: "tuned", Type: QuerylogSinkSlog, QueueSize: 500, BatchSize: 50, FlushInterval: Duration(2 * time.Second), Timeout: Duration(30 * time.Second), Attempts: 1, RetryBackoff: Duration(500 * time.Millisecond)}}
	if !reflect.DeepEqual(cfg.Querylog.Sinks, want) {
		t.Fatalf("sinks: %+v", cfg.Querylog.Sinks)
	}
	again, err := FromFlat(Flatten(cfg))
	if err != nil || !reflect.DeepEqual(again.Querylog.Sinks, want) {
		t.Fatalf("flat round trip: %+v, %v", again, err)
	}
	minimal := *cfg
	minimal.Querylog.Sinks = []QuerylogSink{{Name: "log-lines", Type: QuerylogSinkSlog}}
	flat := Flatten(&minimal)
	if got := flat["querylog.sinks"]; !reflect.DeepEqual(got, []any{map[string]any{"name": "log-lines", "type": "slog"}}) {
		t.Fatalf("unset limits must stay unset in the flat form: %#v", got)
	}
	if changed := Changed(cfg, &minimal); !reflect.DeepEqual(changed, []string{"querylog.sinks"}) {
		t.Fatalf("changed: %v", changed)
	}
	next, _, err := ApplyClusterDocument(cfg, map[string]any{"querylog": map[string]any{"sinks": []any{map[string]any{"name": "published", "type": "slog", "queueSize": 200}}}})
	if err != nil || len(next.Querylog.Sinks) != 1 || next.Querylog.Sinks[0].Name != "published" || next.Querylog.Sinks[0].QueueSize != 200 {
		t.Fatalf("cluster document: %+v, %v", next, err)
	}
	if k, ok := Lookup("querylog.sinks"); !ok || k.RestartRequired || k.HostOnly || k.Scope != ScopeCluster || !PublicClusterKey(k) {
		t.Fatalf("querylog.sinks must be a hot public cluster key: %+v", k)
	}
}

func TestQuerylogSinksValidation(t *testing.T) {
	valid := func() *Config {
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	for name, tc := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"missing name":       {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Type: "slog"}} }, "querylog.sinks[0].name is required"},
		"missing type":       {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a"}} }, "querylog.sinks[0].type is required"},
		"unknown type":       {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "kafka"}} }, `querylog.sinks[0].type: unknown value "kafka" (slog, clickhouse, loki)`},
		"reserved name":      {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "sqlite", Type: "slog"}} }, `querylog.sinks[0].name: "sqlite" names the primary sink`},
		"reserved slog name": {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "slog", Type: "slog"}} }, `querylog.sinks[0].name: "slog" names the primary sink`},
		"bad name":           {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "Log Lines", Type: "slog"}} }, `querylog.sinks[0].name: "Log Lines" is not a lowercase name`},
		"long name":          {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: strings.Repeat("a", 33), Type: "slog"}} }, "is not a lowercase name"},
		"primary is slog": {func(c *Config) {
			c.Querylog.Sink = "slog"
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog"}}
		}, "querylog.sinks[0].type: a second slog sink would write every query twice"},
		"queue too small": {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", QueueSize: 99}} }, "querylog.sinks[0].queueSize: 99 is outside 100-100000"},
		"queue too large": {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", QueueSize: 100001}} }, "querylog.sinks[0].queueSize: 100001 is outside 100-100000"},
		"batch too large": {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", BatchSize: 10001}} }, "querylog.sinks[0].batchSize: 10001 is outside 1-10000"},
		"batch over queue": {func(c *Config) {
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", QueueSize: 100, BatchSize: 101}}
		}, "querylog.sinks[0].batchSize: 101 is larger than the queue of 100"},
		"attempts":          {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", Attempts: 11}} }, "querylog.sinks[0].attempts: 11 is outside 1-10"},
		"negative attempts": {func(c *Config) { c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", Attempts: -1}} }, "querylog.sinks[0].attempts: -1 is outside 1-10"},
		"flush interval": {func(c *Config) {
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", FlushInterval: Duration(time.Millisecond)}}
		}, "querylog.sinks[0].flushInterval: 1ms is outside 100ms-1m0s"},
		"timeout": {func(c *Config) {
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", Timeout: Duration(time.Hour)}}
		}, "querylog.sinks[0].timeout: 1h0m0s is outside 1s-2m0s"},
		"backoff": {func(c *Config) {
			c.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog", RetryBackoff: Duration(-time.Second)}}
		}, "querylog.sinks[0].retryBackoff: -1s is outside 100ms-1m0s"},
		"too many": {func(c *Config) {
			for i := range MaxQuerylogSinks + 1 {
				c.Querylog.Sinks = append(c.Querylog.Sinks, QuerylogSink{Name: "s" + string(rune('a'+i)), Type: "future"})
			}
		}, "querylog.sinks: 9 sinks, at most 8 are supported"},
	} {
		cfg := valid()
		tc.edit(cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	cfg := valid()
	cfg.Querylog.Sinks = []QuerylogSink{{Name: "a", Type: "slog"}, {Name: "a", Type: "future"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `querylog.sinks[1].name: duplicate sink name "a"`) {
		t.Errorf("duplicate name: %v", err)
	}
	cfg = valid()
	cfg.Querylog.Enabled = false
	cfg.Querylog.Sinks = []QuerylogSink{{Name: "kept-while-off", Type: "slog", QueueSize: 100, BatchSize: 100, FlushInterval: Duration(time.Minute), Timeout: Duration(time.Second), Attempts: 10, RetryBackoff: Duration(time.Minute)}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("bounds are inclusive and sinks stay configured while the log is off: %v", err)
	}
}

func TestQuerylogRawMinimum(t *testing.T) {
	if _, err := Load("", []string{"KUDURRU_QUERYLOG__RETENTION__RAW=30m"}); err == nil || !strings.Contains(err.Error(), "shorter than 1h") {
		t.Errorf("30m raw retention: %v", err)
	}
	if _, err := Load("", []string{"KUDURRU_QUERYLOG__RETENTION__RAW=0s"}); err != nil {
		t.Errorf("size-only retention rejected: %v", err)
	}
}

func TestByteSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ByteSize
		out  string
	}{
		{"10GiB", 10 << 30, "10GiB"},
		{"1536MiB", 1536 << 20, "1536MiB"},
		{"2GB", 2e9, "1953125KiB"},
		{"4096", 4096, "4KiB"},
		{"0", 0, "0"},
		{" 3 KiB ", 3 << 10, "3KiB"},
		{"100B", 100, "100"},
	} {
		var b ByteSize
		if err := b.UnmarshalText([]byte(tc.in)); err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if b != tc.want || b.String() != tc.out {
			t.Errorf("%q: got %d (%s), want %d (%s)", tc.in, b, b, tc.want, tc.out)
		}
	}
	for _, bad := range []string{"", "GiB", "1.5GiB", "ten", "99999999999TiB"} {
		var b ByteSize
		if err := b.UnmarshalText([]byte(bad)); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
