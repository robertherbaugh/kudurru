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
