package blocking

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// MaxStaleExcludes bounds SourceStats.StaleExcludes.
const MaxStaleExcludes = 50

// ExcludeDigest is a digest of a source's exclude set (order and
// duplicates do not matter); "" for none. Fingerprints include it, so a
// changed exclusion recompiles the list.
func ExcludeDigest(exclude []string) string {
	if len(exclude) == 0 {
		return ""
	}
	v := slices.Clone(exclude)
	slices.Sort(v)
	v = slices.Compact(v)
	h := sha256.Sum256([]byte(strings.Join(v, "\n")))
	return hex.EncodeToString(h[:8])
}

// ExcludeKey is the pattern a rendered rule carries (Rule.Pattern): the
// rule without "@@", the adblock anchors, "*." and the regex slashes.
func ExcludeKey(rule string) string {
	s := strings.TrimPrefix(rule, "@@")
	switch {
	case len(s) > 2 && s[0] == '/' && s[len(s)-1] == '/':
		return s[1 : len(s)-1]
	case strings.HasPrefix(s, "||*.") && strings.HasSuffix(s, "^"):
		return s[4 : len(s)-1]
	case strings.HasPrefix(s, "||") && strings.HasSuffix(s, "^"):
		return s[2 : len(s)-1]
	case strings.HasPrefix(s, "|") && strings.HasSuffix(s, "^"):
		return s[1 : len(s)-1]
	case strings.HasPrefix(s, "*."):
		return s[2:]
	}
	return s
}

// excluder drops the rules of one source that Source.Exclude names. The
// pattern index keeps the common case (a rule nobody excluded) to one map
// lookup without rendering the rule.
type excluder struct {
	keys       map[string]struct{}
	rules      map[string]bool // rendered rule -> matched
	count      int
	exceptions int
}

func newExcluder(exclude []string) *excluder {
	if len(exclude) == 0 {
		return nil
	}
	ex := &excluder{keys: make(map[string]struct{}, len(exclude)), rules: make(map[string]bool, len(exclude))}
	for _, e := range exclude {
		ex.keys[ExcludeKey(e)] = struct{}{}
		ex.rules[e] = false
	}
	return ex
}

func (ex *excluder) excluded(r parser.Rule) bool {
	if _, ok := ex.keys[r.Pattern]; !ok {
		return false
	}
	s := r.String()
	if _, ok := ex.rules[s]; !ok {
		return false
	}
	ex.rules[s] = true
	ex.count++
	if r.Exception {
		ex.exceptions++
	}
	return true
}

func (ex *excluder) stale() []string {
	var out []string
	for e, hit := range ex.rules {
		if !hit {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	if len(out) > MaxStaleExcludes {
		out = out[:MaxStaleExcludes]
	}
	return out
}
