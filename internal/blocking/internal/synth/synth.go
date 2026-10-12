// Package synth generates deterministic, realistic-looking domain names and
// block lists for the blocking benchmarks and tests.
//
// The shape approximates public block lists: about half the entries are
// registrable domains, the rest have one to three extra labels; TLDs follow
// a skewed distribution with .com first; labels are 5-19 characters of
// [a-z0-9] with occasional hyphens. The average generated name is about 23
// bytes, slightly above the 22.8 bytes of the StevenBlack sample in
// testdata, so memory measurements err on the high side.
package synth

import (
	"bufio"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
)

// tlds is a weighted TLD table (weights in per-mille); the remainder goes
// to random two-letter ccTLDs.
var tlds = []struct {
	tld    string
	weight int
}{
	{"com", 480}, {"net", 90}, {"org", 40}, {"io", 30}, {"xyz", 30}, {"info", 25},
	{"ru", 25}, {"de", 20}, {"cn", 20}, {"top", 20}, {"online", 15}, {"site", 15},
	{"co", 15}, {"uk", 15}, {"br", 10}, {"fr", 10}, {"jp", 10}, {"in", 10},
	{"pl", 10}, {"nl", 10}, {"app", 10},
}

var commonPrefixes = []string{"www", "ads", "ad", "cdn", "track", "pixel", "stats", "api", "img", "static", "m", "t", "s1", "eu", "us"}

const (
	alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	letters  = "abcdefghijklmnopqrstuvwxyz"
)

// Gen is a deterministic name generator. It is not safe for concurrent use.
type Gen struct {
	r  *rand.Rand
	sb strings.Builder
}

// New returns a generator seeded with seed.
func New(seed uint64) *Gen {
	return &Gen{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // deterministic benchmark data
}

// TLD returns a TLD from the weighted table.
func (g *Gen) TLD() string {
	x := g.r.IntN(1000)
	for _, t := range tlds {
		if x < t.weight {
			return t.tld
		}
		x -= t.weight
	}
	return string([]byte{letters[g.r.IntN(26)], letters[g.r.IntN(26)]})
}

// Label returns a random label of 5-19 characters.
func (g *Gen) Label() string {
	n := 5 + g.r.IntN(7) + g.r.IntN(9) // 5..19, peaked around 12
	g.sb.Reset()
	for i := range n {
		if i > 0 && i < n-1 && g.r.IntN(12) == 0 {
			g.sb.WriteByte('-')
			continue
		}
		g.sb.WriteByte(alphabet[g.r.IntN(len(alphabet))])
	}
	return g.sb.String()
}

// Prefix returns a subdomain label: a common one ("www", "ads", ...) or a
// random one.
func (g *Gen) Prefix() string {
	if g.r.IntN(5) < 2 {
		return commonPrefixes[g.r.IntN(len(commonPrefixes))]
	}
	return g.Label()
}

// Domain returns a registrable domain with zero to three extra labels.
func (g *Gen) Domain() string {
	d := g.Label() + "." + g.TLD()
	extra := 0
	switch x := g.r.IntN(100); {
	case x >= 96:
		extra = 3
	case x >= 82:
		extra = 2
	case x >= 45:
		extra = 1
	}
	for range extra {
		d = g.Prefix() + "." + d
	}
	return d
}

// IntN exposes the generator's randomness for picking queries.
func (g *Gen) IntN(n int) int { return g.r.IntN(n) }

// Unique returns n distinct domains generated from seed.
func Unique(seed uint64, n int) []string {
	g := New(seed)
	seen := make(map[string]struct{}, n)
	out := make([]string, 0, n)
	for len(out) < n {
		d := g.Domain()
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// Write writes domains as a list in the given format: "hosts"
// (0.0.0.0 name), "adblock" (||name^) or "domains" (name), preceded by a
// short comment header like real lists.
func Write(w io.Writer, format string, domains []string) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	prefix, suffix := "", ""
	switch format {
	case "hosts":
		prefix = "0.0.0.0 "
	case "adblock":
		prefix, suffix = "||", "^"
	case "domains":
	default:
		return fmt.Errorf("synth: unknown format %q", format)
	}
	if _, err := fmt.Fprintf(bw, "# Title: synthetic %s list\n# Entries: %d\n\n", format, len(domains)); err != nil {
		return err
	}
	for _, d := range domains {
		if _, err := bw.WriteString(prefix + d + suffix + "\n"); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// Regexes returns n regex rules with shapes common in regex block lists.
// One in ten has no literal of four bytes, so it cannot be indexed and is
// evaluated for every lookup, as happens with real lists.
func Regexes(seed uint64, n int) []string {
	g := New(seed)
	out := make([]string, 0, n)
	for i := range n {
		l, t := g.Label(), g.TLD()
		if i%10 == 9 {
			out = append(out, fmt.Sprintf(`^[a-z]{%d}[0-9]{%d,}\.`, 3+(i/10)%20, 1+(i/200)%10))
			continue
		}
		switch i % 5 {
		case 0:
			out = append(out, fmt.Sprintf(`^ad[sx]?[0-9]*\.%s\.%s$`, l, t))
		case 1:
			out = append(out, fmt.Sprintf(`(^|\.)%s[0-9]{1,3}\.%s$`, l, t))
		case 2:
			out = append(out, fmt.Sprintf(`^%s-?[a-z]{2,6}\.(com|net)$`, l))
		case 3:
			out = append(out, fmt.Sprintf(`^track(er|ing)?[0-9]*\.%s\.`, l))
		default:
			out = append(out, fmt.Sprintf(`^[a-z0-9]{8,}\.%s\.%s$`, l, t))
		}
	}
	return out
}
