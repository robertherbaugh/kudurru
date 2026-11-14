package config

import "testing"

// TestRawQuerylogChange pins which settings changes need querylog.raw
// (D372): a change of querylog.sinks or of an exclusion while next keeps a
// raw dnstap sink, and nothing else.
func TestRawQuerylogChange(t *testing.T) {
	raw := Querylog{Sinks: []QuerylogSink{{Name: "collector", Type: QuerylogSinkDnstap, Dnstap: &QuerylogDnstap{Target: "unix:/run/dnstap.sock", Raw: true}}}}
	plain := Querylog{Sinks: []QuerylogSink{{Name: "collector", Type: QuerylogSinkDnstap, Dnstap: &QuerylogDnstap{Target: "unix:/run/dnstap.sock"}}}}
	for _, tc := range []struct {
		changed []string
		next    Querylog
		want    bool
	}{
		{[]string{QuerylogSinksKey}, raw, true},
		{[]string{"querylog.exclude.clients"}, raw, true},
		{[]string{"querylog.exclude.domains"}, raw, true},
		{[]string{"querylog.exclude.sudn"}, raw, true},
		{[]string{"querylog.exclude"}, raw, true},
		{[]string{"cache.maxEntries", "querylog.exclude.sudn"}, raw, true},
		{[]string{"querylog.privacy"}, raw, false},
		{[]string{"querylog.excluded"}, raw, false},
		{nil, raw, false},
		{[]string{QuerylogSinksKey, "querylog.exclude.clients"}, plain, false},
		{[]string{"querylog.exclude.clients"}, Querylog{}, false},
	} {
		if got := RawQuerylogChange(tc.changed, tc.next); got != tc.want {
			t.Errorf("RawQuerylogChange(%v, raw=%v) = %v, want %v", tc.changed, len(RawQuerylogSinks(tc.next)) > 0, got, tc.want)
		}
	}
}
