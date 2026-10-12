package zone

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func TestCanonicalKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		err  bool
	}{
		{name: ".", key: ""},
		{name: "", key: ""},
		{name: "com.", key: "com\x00"},
		{name: "com", key: "com\x00"},
		{name: "WWW.Example.COM.", key: "com\x00example\x00www\x00"},
		{name: `a\.b.example.`, key: "example\x00a.b\x00"},
		{name: `\065.example.`, key: "example\x00a\x00"},
		{name: `\000.example.`, key: "example\x00\x01\x01\x00"},
		{name: `\001.example.`, key: "example\x00\x01\x02\x00"},
		{name: `*.example.`, key: "example\x00*\x00"},
		{name: "a..example.", err: true},
		{name: `trailing\`, err: true},
		{name: `\256.example.`, err: true},
		{name: strings.Repeat("a", 64) + ".example.", err: true},
		{name: strings.Repeat("abcdefghi.", 26), err: true}, // 261 octets
	} {
		got, err := canonicalKey(tc.name)
		if (err != nil) != tc.err {
			t.Errorf("%q: err = %v, want error %v", tc.name, err, tc.err)
			continue
		}
		if !tc.err && got != tc.key {
			t.Errorf("%q: key %q, want %q", tc.name, got, tc.key)
		}
	}
}

// TestCanonicalOrder checks the RFC 4034 §6.1 example ordering.
func TestCanonicalOrder(t *testing.T) {
	ordered := []string{
		"example.",
		"a.example.",
		"yljkjljk.a.example.",
		"Z.a.example.",
		`zABC.a.EXAMPLE.`,
		"z.example.",
		`\001.z.example.`,
		"*.z.example.",
		`\200.z.example.`,
	}
	keys := make([]string, len(ordered))
	for i, n := range ordered {
		k, err := canonicalKey(n)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = k
	}
	if !slices.IsSorted(keys) {
		t.Fatalf("keys are not in canonical order: %q", keys)
	}
}

func TestKeyPrefixIsAncestry(t *testing.T) {
	k1, _ := canonicalKey("www.example.org.")
	k2, _ := canonicalKey("example.org.")
	k3, _ := canonicalKey("ample.org.")
	if !isKeyPrefix(k1, k2) || isKeyPrefix(k1, k3) || !isKeyPrefix(k1, "") {
		t.Fatal("key prefixes must follow label boundaries")
	}
	if keyLabels(k1) != 3 || keyLabels("") != 0 {
		t.Fatal("label count")
	}
}

func TestReplaceSuffix(t *testing.T) {
	for _, tc := range []struct {
		qname, owner, target, want string
		ok                         bool
	}{
		{"a.b.dn.example.", "dn.example.", "other.net.", "a.b.other.net.", true},
		{`a\.b.dn.example.`, "dn.example.", "other.net.", `a\.b.other.net.`, true},
		{"x.example.", ".", "corp.", "x.example.corp.", true},
		{"x.dn.example.", "dn.example.", ".", "x.", true},
		{"dn.example.", "dn.example.", "other.", "", false},
		{longLabel + "." + longLabel + ".dn.", "dn.", longLabel + "." + longLabel + ".", "", false},
	} {
		got, ok := replaceSuffix(tc.qname, tc.owner, tc.target)
		if ok != tc.ok || got != tc.want {
			t.Errorf("replaceSuffix(%q, %q, %q) = %q, %v; want %q, %v", tc.qname, tc.owner, tc.target, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTreeBalancedAndOrdered(t *testing.T) {
	var tr tree
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic test data
	const n = 50000
	for i := range n {
		name := fmt.Sprintf("h%d.s%d.example.", r.IntN(1<<30), i%97)
		k, err := canonicalKey(name)
		if err != nil {
			t.Fatal(err)
		}
		tr.upsert(k, name)
	}
	var keys []string
	tr.walk(func(nd *node) bool { keys = append(keys, nd.key); return true })
	if len(keys) != tr.count || !slices.IsSorted(keys) {
		t.Fatalf("walk returned %d keys (count %d), sorted=%v", len(keys), tr.count, slices.IsSorted(keys))
	}
	// An LLRB tree has height at most 2*log2(n+1).
	if h, limit := tr.height(), 2*17; h > limit {
		t.Fatalf("height %d exceeds %d", h, limit)
	}
	for _, k := range keys[:100] {
		if tr.search(k) == nil {
			t.Fatalf("key %q not found", k)
		}
	}
	if tr.search("zzz\x00") != nil {
		t.Fatal("found a missing key")
	}
	// Walking stops when asked to.
	seen := 0
	tr.walk(func(*node) bool { seen++; return seen < 10 })
	if seen != 10 {
		t.Fatalf("walk did not stop: %d", seen)
	}
}

func FuzzCanonicalKey(f *testing.F) {
	for _, s := range []string{"example.org.", `a\.b.`, `\065\066.c`, "*.x.", ".", `\\.`, "a..b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		k, err := canonicalKey(name)
		if err != nil {
			return
		}
		// Keys never contain an unescaped separator inside a label, so the
		// label count equals the separator count, and appendKey agrees.
		b, err := appendKey(nil, name)
		if err != nil || string(b) != k {
			t.Fatalf("appendKey disagrees for %q", name)
		}
		if keyLabels(k) > maxLabels {
			t.Fatalf("too many labels for %q", name)
		}
		// The canonical spelling names the same key and is a fixed point.
		n := nameFromKey(k)
		if k2, err := canonicalKey(n); err != nil || k2 != k {
			t.Fatalf("nameFromKey(%q) = %q does not re-key: %v", name, n, err)
		}
		if canonicalName(n) != n {
			t.Fatalf("canonicalName is not idempotent for %q", n)
		}
	})
}

func TestCanonicalName(t *testing.T) {
	for in, want := range map[string]string{
		".":                    ".",
		"":                     ".",
		"Example.ORG":          "example.org.",
		`\069xample.org.`:      "example.org.",
		`a\.b.example.`:        `a\.b.example.`,
		`sp\032ace.example.`:   `sp\ ace.example.`,
		`sp\ ace.example.`:     `sp\ ace.example.`,
		`t\009ab.example.`:     `t\009ab.example.`,
		`caf\195\169.example.`: `caf\195\169.example.`,
		`q\"uote\;semi.ex.`:    `q\"uote\;semi.ex.`,
		"*.Wild.example.":      "*.wild.example.",
		"a..b":                 "a..b.", // invalid: fqdn fallback
	} {
		if got := canonicalName(in); got != want {
			t.Errorf("canonicalName(%q) = %q, want %q", in, got, want)
		}
	}
	// The spelling is the one miekg/dns prints, so exported owners are
	// stable across a parse.
	for _, n := range []string{`sp\032ace.example.`, `a\.b.example.`, `t\009ab.example.`, `x\@y.example.`, `caf\195\169.example.`} {
		r, err := dns.NewRR(n + " 1 IN A 192.0.2.1")
		if err != nil {
			t.Fatal(err)
		}
		if printed := strings.SplitN(r.String(), "\t", 2)[0]; printed != canonicalName(n) {
			t.Errorf("miekg prints %q, canonicalName gives %q", printed, canonicalName(n))
		}
	}
}

// TestMailbox: an e-mail rname is split at its last unescaped @; an escaped
// \@ (a presentation-form RNAME as an import stores it) stays in its label
// (FuzzImportBIND: "@ SOA 0 @@0 0" exported \@.0.example.com. and
// re-imported as \..0.example.com.).
func TestMailbox(t *testing.T) {
	for in, want := range map[string]string{
		"hostmaster@example.com":   `hostmaster.example.com.`,
		"first.last@example.com":   `first\.last.example.com.`,
		`hostmaster.example.com.`:  `hostmaster.example.com.`,
		`\@.0.example.com.`:        `\@.0.example.com.`,
		`a\@b@example.com`:         `a\@b.example.com.`,
		`a\064b.example.com.`:      `a\064b.example.com.`,
		`first\.last.example.com.`: `first\.last.example.com.`,
	} {
		got, err := mailbox(in)
		if err != nil || got != want {
			t.Errorf("mailbox(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
