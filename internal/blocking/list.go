package blocking

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// Mode says whether a list's entries block or allow. The values are those of
// api/v1alpha1.ListMode.
type Mode string

// Modes.
const (
	ModeBlock Mode = "block"
	ModeAllow Mode = "allow"
)

// DefaultRegexHardCap is the per-list regex cap used when
// CompileOptions.RegexHardCap is zero. Regexes beyond it are dropped and
// counted; matcher.RegexWarnThreshold (500) triggers a warning first.
const DefaultRegexHardCap = 1000

// keepUnsupportedPerList bounds the unsupported rules a List retains for
// UI diagnostics only; execution and counting are never bounded.
const keepUnsupportedPerList = 1000

// Errors returned by Compile for a malformed Spec.
var (
	ErrNoName    = errors.New("blocking: list has no name")
	ErrBadMode   = errors.New("blocking: list mode must be block or allow")
	ErrNoSources = errors.New("blocking: list has no sources")
)

// Source is one input of a list. Open is called up to twice per compile:
// once for fingerprint/badfilter collection, once to build the matchers.
type Source struct {
	Name   string
	Format parser.Format
	// HostsSubdomains makes hosts and plain-domain entries of this source
	// match subdomains too (see parser.Options).
	HostsSubdomains bool
	// Exclude holds rules of this source, as Rule.String renders them
	// ("ads.example.com", "||ads.example.com^", "/re/"), that are not
	// compiled: the entries a user switched off
	// (spec.sources[].exclude).
	Exclude []string
	Open    func() (io.ReadCloser, error)
}

// BytesSource is a Source over an in-memory body.
func BytesSource(name string, format parser.Format, body []byte) Source {
	return Source{Name: name, Format: format, Open: func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}}
}

// LinesSource is a Source over inline lines, as in ListSource.Inline.
func LinesSource(name string, format parser.Format, lines []string) Source {
	return BytesSource(name, format, []byte(strings.Join(lines, "\n")))
}

// FileSource is a Source over a file, such as the list cache entry
// lists/<sha256(url)> written by the list fetcher.
func FileSource(name string, format parser.Format, path string) Source {
	return Source{Name: name, Format: format, Open: func() (io.ReadCloser, error) {
		return os.Open(path) //nolint:gosec // the path is the operator's list cache, not request input
	}}
}

// Spec describes one list to compile.
type Spec struct {
	Name    string
	Mode    Mode
	Sources []Source
}

func (s *Spec) validate() error {
	switch {
	case s.Name == "":
		return ErrNoName
	case s.Mode != ModeBlock && s.Mode != ModeAllow:
		return fmt.Errorf("%w (list %q has %q)", ErrBadMode, s.Name, s.Mode)
	case len(s.Sources) == 0:
		return fmt.Errorf("%w (list %q)", ErrNoSources, s.Name)
	}
	for _, src := range s.Sources {
		if src.Open == nil {
			return fmt.Errorf("blocking: list %q source %q has no Open function", s.Name, src.Name)
		}
		if !src.Format.Valid() {
			return fmt.Errorf("blocking: list %q source %q: %w %q", s.Name, src.Name, parser.ErrUnknownFormat, src.Format)
		}
	}
	return nil
}

// CompileOptions tune Compile.
type CompileOptions struct {
	// RegexHardCap is the maximum number of regex rules per list (primary
	// and exception rules together): 0 means DefaultRegexHardCap, negative
	// means unlimited.
	RegexHardCap int
}

func (o CompileOptions) regexCap() int {
	switch {
	case o.RegexHardCap == 0:
		return DefaultRegexHardCap
	case o.RegexHardCap < 0:
		return 0
	}
	return o.RegexHardCap
}

// SourceStats reports one source of a compiled list.
type SourceStats struct {
	Name   string
	Format parser.Format // effective (detected when the source said auto)
	Title  string
	// CRC32 is the checksum of the normalized rule lines.
	CRC32       uint32
	Lines       int
	Rules       int
	Exceptions  int
	Unsupported int
	Invalid     int
	Ignored     int
	// Excluded counts the rules Source.Exclude switched off; they are not
	// in Rules.
	Excluded int
	// StaleExcludes are the Source.Exclude values that matched no rule
	// (sorted, at most MaxStaleExcludes).
	StaleExcludes      []string
	UnsupportedReasons map[string]int
	InvalidReasons     map[string]int
	InvalidSamples     []parser.Diagnostic
	// Err is set when the source could not be read or is not a list (HTML,
	// binary); its rules are then missing from the List.
	Err error
}

// Stats summarizes a compiled list. Entries, RuleCount and Invalid map to
// BlockList.status.{entries,ruleCount,invalid}.
type Stats struct {
	// Entries is the number of unique compiled entries over every matcher
	// of both chains.
	Entries int
	// RuleCount is the number of rules the parser accepted, duplicates
	// included.
	RuleCount int
	// Exceptions is the number of unique compiled @@ entries of a block
	// list (an allow list folds its @@ rules into its primary entries).
	Exceptions int
	// Duplicates is RuleCount minus the unique entries and dropped rules.
	Duplicates int
	// Invalid counts lines that are not DNS rules, plus regexes the engine
	// rejected.
	Invalid int
	// Unsupported counts valid rules this version does not compile
	// (geolocation, CIDRs, hosts rewrites).
	Unsupported int
	// Ignored counts hosts-file boilerplate (localhost and friends).
	Ignored int
	// Regexes is the number of compiled regex rules; RegexUnindexed how many
	// of them have no indexable required literal and can run for every lookup;
	// RegexDropped the number cut by the hard cap.
	Regexes        int
	RegexUnindexed int
	RegexDropped   int
	// Bytes approximates the heap footprint of the compiled matchers.
	Bytes int
	// CompileTime is the wall time Compile spent building this list.
	CompileTime time.Duration
}

// List is one compiled BlockList. It is immutable and safe for concurrent
// use.
type List struct {
	name        string
	mode        Mode
	primary     chain
	except      chain
	stats       Stats
	sources     []SourceStats
	unsupported []parser.UnsupportedRule
	warnings    []string
	fingerprint uint32
	// complete is false when a source failed; such a list is never reused
	// by the checksum skip.
	complete    bool
	slow        []*compiledRule
	slowIndex   *matcher.PatternIndex
	hasRewrites bool
	badfilters  map[string]bool
}

// Name returns the list name.
func (l *List) Name() string { return l.name }

// Mode returns whether the list blocks or allows.
func (l *List) Mode() Mode { return l.mode }

// Stats returns the compile statistics.
func (l *List) Stats() Stats { return l.stats }

// Sources returns per-source statistics in Spec order.
func (l *List) Sources() []SourceStats {
	out := slices.Clone(l.sources)
	for i := range out {
		out[i].StaleExcludes = slices.Clone(out[i].StaleExcludes)
		out[i].UnsupportedReasons = maps.Clone(out[i].UnsupportedReasons)
		out[i].InvalidReasons = maps.Clone(out[i].InvalidReasons)
		out[i].InvalidSamples = slices.Clone(out[i].InvalidSamples)
	}
	return out
}

// Unsupported returns up to 1000 rules this version parsed but did not
// compile, with their tokenized modifiers, for UI diagnostics only.
func (l *List) Unsupported() []parser.UnsupportedRule {
	out := slices.Clone(l.unsupported)
	for i := range out {
		out[i].Modifiers = slices.Clone(out[i].Modifiers)
	}
	return out
}

// Warnings returns human-readable compile warnings (regex counts, caps,
// unsupported rules, failed sources).
func (l *List) Warnings() []string { return slices.Clone(l.warnings) }

// Fingerprint returns the CRC-32 over the list mode, options and every
// source's rule-line checksum; Compile reuses a previous List with the same
// fingerprint.
func (l *List) Fingerprint() uint32 { return l.fingerprint }

// Match reports whether the list's primary entries (deny entries of a block
// list, allow entries of an allow list) match name.
func (l *List) Match(name string) (Match, bool) {
	return l.MatchContext(Context{Name: name, Type: 1, Class: 1})
}

// MatchContext reports a primary deny/allow entry using question/client
// selectors. Rewrites have their own typed Selection.EvaluateContext action.
func (l *List) MatchContext(ctx Context) (Match, bool) { return l.matchContext(ctx, false) }
func (l *List) matchContext(ctx Context, except bool) (Match, bool) {
	ctx.Name = NormalizeName(ctx.Name)
	ch := &l.primary
	if except {
		ch = &l.except
	}
	m, ok := ch.match(ctx.Name, matcher.Hash(ctx.Name))
	if ok {
		return m, true
	}
	best := candidate{rank: -1}
	l.slowIndex.Visit(ctx.Name, func(id int) {
		r := l.slow[id]
		if r.rewrite != nil || !r.matches(ctx) || (r.exception && l.mode == ModeBlock) != except {
			return
		}
		c := candidateFor(l, r, ctx.Name)
		if better(c, best) {
			best = c
		}
	})
	return best.decision.Match, best.rank >= 0
}

// MatchException reports whether one of the list's own @@ rules matches
// name. It is always false for allow lists.
func (l *List) MatchException(name string) (Match, bool) {
	return l.MatchExceptionContext(Context{Name: name, Type: 1, Class: 1})
}

// MatchExceptionContext checks ordinary @@ entries with question selectors.
func (l *List) MatchExceptionContext(ctx Context) (Match, bool) { return l.matchContext(ctx, true) }

// MatchIP reports whether an IP entry of the primary chain matches a.
func (l *List) MatchIP(a netip.Addr) (Match, bool) {
	return l.primary.matchIP(a)
}

// Compile builds a List from spec. When prev is a complete List whose
// fingerprint equals the new one (no rule line, option or mode changed),
// Compile returns prev itself without rebuilding matchers, so callers detect the skip
// with l == prev.
//
// A source that cannot be read or is not a list (parser.ErrHTML,
// parser.ErrBinary) is recorded in SourceStats.Err and in the returned
// error; the List is still built from the other sources. When every source
// fails, Compile returns a nil List. Rebuild keeps the previous List
// whenever Compile returns an error.
func Compile(spec Spec, prev *List, opts CompileOptions) (*List, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	start := time.Now()
	regexCap := opts.regexCap()
	fp, badfilters, fpErr := fingerprint(&spec, regexCap)
	if fpErr == nil && prev != nil && prev.complete && prev.fingerprint == fp {
		return prev, nil
	}

	c := newCompiler(spec.Mode, regexCap)
	c.badfilters = badfilters
	l := &List{name: spec.Name, mode: spec.Mode, fingerprint: fp}
	var errs []error
	ok := 0
	for _, src := range spec.Sources {
		st, err := c.source(src)
		l.sources = append(l.sources, st)
		if err != nil {
			errs = append(errs, fmt.Errorf("blocking: list %q source %q: %w", spec.Name, src.Name, err))
			l.warnings = append(l.warnings, fmt.Sprintf("source %q failed: %v", src.Name, err))
			continue
		}
		ok++
	}
	if ok == 0 {
		return nil, errors.Join(errs...)
	}
	c.build(l)
	l.complete = fpErr == nil && len(errs) == 0
	l.stats.CompileTime = time.Since(start)
	return l, errors.Join(errs...)
}

// fingerprint hashes normalized rule lines and collects all badfilter
// directives before compilation. Comment-only changes do not force a rebuild.
func fingerprint(spec *Spec, regexCap int) (uint32, map[string]bool, error) {
	h := crc32.NewIEEE()
	_, _ = fmt.Fprintf(h, "kudurru-blocking-v2\x00%s\x00%d\x00", spec.Mode, regexCap)
	var badfilters map[string]bool
	for _, src := range spec.Sources {
		rc, err := src.Open()
		if err != nil {
			return 0, nil, err
		}
		excluded := map[string]bool{}
		for _, text := range src.Exclude {
			excluded[text] = true
		}
		res, err := parser.Parse(rc, parser.Options{Format: src.Format, HostsSubdomains: src.HostsSubdomains, KeepUnsupported: -1, KeepInvalid: -1, EmitSlow: func(u parser.UnsupportedRule) error {
			if excluded[u.Text] {
				return parser.ErrUnsupportedSlow
			}
			for _, m := range u.Modifiers {
				if m.Name == "badfilter" {
					r, e := compileRule(u)
					if e == nil && r.badfilter {
						if badfilters == nil {
							badfilters = map[string]bool{}
						}
						badfilters[r.key] = true
					}
					break
				}
			}
			return parser.ErrUnsupportedSlow
		}}, nil)
		_ = rc.Close()
		if err != nil {
			return 0, nil, err
		}
		format := src.Format
		if format == "" {
			format = parser.FormatAuto
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%t\x00%08x\x00%s\x00", src.Name, format, src.HostsSubdomains, res.CRC32, ExcludeDigest(src.Exclude))
	}
	return h.Sum32(), badfilters, nil
}

type chainBuilder struct {
	exact  matcher.ExactBuilder
	suffix matcher.SuffixBuilder
	regex  matcher.RegexBuilder
	ips    matcher.IPSetBuilder
}

func (b *chainBuilder) build(exception bool) chain {
	return chain{
		exact:     b.exact.Build(),
		suffix:    b.suffix.Build(),
		regex:     b.regex.Build(),
		ips:       b.ips.Build(),
		exception: exception,
	}
}

func (b *chainBuilder) added() int {
	return b.exact.Added() + b.suffix.Added() + b.ips.Added()
}

type compiler struct {
	mode            Mode
	regexCap        int
	primary, except chainBuilder

	regexAccepted int // Add calls that did not fail, duplicates included
	regexDropped  int
	regexInvalid  int
	rejected      int // names the matchers refused (cannot happen with parser output)

	ruleCount, unsupported, invalid, ignored int
	unsupportedRules                         []parser.UnsupportedRule
	unsupportedReasons                       map[string]int
	slow                                     []*compiledRule
	badfilters                               map[string]bool
	slowRegex, slowAdded                     int
	slowSeen                                 map[string]bool
}

func newCompiler(mode Mode, regexCap int) *compiler {
	c := &compiler{mode: mode, regexCap: regexCap, unsupportedReasons: map[string]int{}}
	for _, b := range []*chainBuilder{&c.primary, &c.except} {
		b.exact.MergeTags, b.suffix.MergeTags = true, true
	}
	return c
}

func (c *compiler) source(src Source) (SourceStats, error) {
	st := SourceStats{Name: src.Name}
	rc, err := src.Open()
	if err != nil {
		st.Err = err
		return st, err
	}
	defer func() { _ = rc.Close() }()
	emit := c.add
	ex := newExcluder(src.Exclude)
	if ex != nil {
		emit = func(r parser.Rule) error {
			if ex.excluded(r) {
				return nil
			}
			return c.add(r)
		}
	}
	res, err := parser.Parse(rc, parser.Options{Format: src.Format, HostsSubdomains: src.HostsSubdomains, EmitSlow: func(u parser.UnsupportedRule) error {
		if ex != nil {
			if _, ok := ex.rules[u.Text]; ok {
				ex.rules[u.Text] = true
				ex.count++
				if u.Exception {
					ex.exceptions++
				}
				return nil
			}
		}
		return c.addSlow(u)
	}}, emit)
	if err != nil {
		st.Err = err
		return st, err
	}
	if ex != nil {
		res.Rules -= ex.count
		res.Exceptions -= ex.exceptions
		st.Excluded, st.StaleExcludes = ex.count, ex.stale()
	}
	st.Format, st.Title, st.CRC32 = res.Format, res.Title, res.CRC32
	st.Lines, st.Rules, st.Exceptions = res.Lines, res.Rules, res.Exceptions
	st.Unsupported, st.Invalid, st.Ignored = res.Unsupported, res.Invalid, res.Ignored
	st.UnsupportedReasons, st.InvalidReasons, st.InvalidSamples = res.UnsupportedReasons, res.InvalidReasons, res.InvalidSamples

	c.ruleCount += res.Rules
	c.unsupported += res.Unsupported
	c.invalid += res.Invalid
	c.ignored += res.Ignored
	for reason, n := range res.UnsupportedReasons {
		c.unsupportedReasons[reason] += n
	}
	room := keepUnsupportedPerList - len(c.unsupportedRules)
	c.unsupportedRules = append(c.unsupportedRules, res.UnsupportedRules[:min(room, len(res.UnsupportedRules))]...)
	return st, nil
}

// add routes one parsed rule into the right matcher builder.
func (c *compiler) add(r parser.Rule) error {
	if r.Syntax != parser.SyntaxHosts && len(c.badfilters) > 0 && c.badfilters[r.String()+"$"] {
		return nil
	}
	b := &c.primary
	if r.Exception && c.mode == ModeBlock {
		b = &c.except
	}
	tag := syntaxMask(r.Syntax)
	switch r.Kind {
	case parser.KindExact:
		if !b.exact.Add(r.Pattern, tag) {
			c.rejected++
		}
	case parser.KindSuffix:
		if !b.suffix.Add(r.Pattern, tag) {
			c.rejected++
		}
	case parser.KindIP:
		if !b.ips.Add(r.Addr, tag) {
			c.rejected++
		}
	case parser.KindRegex:
		if !b.regex.Has(r.Pattern) && c.regexCap > 0 && c.primary.regex.Len()+c.except.regex.Len()+c.slowRegex >= c.regexCap {
			c.regexDropped++
			return nil
		}
		if err := b.regex.Add(r.Pattern, tag); err != nil {
			c.regexInvalid++
			return nil
		}
		c.regexAccepted++
	}
	return nil
}

func (c *compiler) addSlow(u parser.UnsupportedRule) error {
	r, err := compileRule(u)
	if err != nil {
		return err
	}
	if r.badfilter || c.badfilters[r.key] {
		return nil
	}
	c.slowAdded++
	if c.slowSeen[r.key] {
		return nil
	}
	if strings.HasPrefix(u.Pattern, "/") {
		if c.regexCap > 0 && c.primary.regex.Len()+c.except.regex.Len()+c.slowRegex >= c.regexCap {
			c.regexDropped++
			c.slowAdded--
			return nil
		}
		c.slowRegex++
	}
	if c.slowSeen == nil {
		c.slowSeen = map[string]bool{}
	}
	c.slowSeen[r.key] = true
	r.order = len(c.slow)
	c.slow = append(c.slow, r)
	return nil
}
func (c *compiler) slowRegexes() int {
	return c.slowRegex
}

func (c *compiler) build(l *List) {
	added := c.primary.added() + c.except.added() + c.regexAccepted
	l.primary = c.primary.build(false)
	l.except = c.except.build(true)
	l.slow, l.badfilters = c.slow, c.badfilters
	var patterns []*matcher.Pattern
	slowExceptions, slowUnindexed := 0, 0
	for _, r := range l.slow {
		l.hasRewrites = l.hasRewrites || r.rewrite != nil
		patterns = append(patterns, r.pattern)
		if r.exception && c.mode == ModeBlock {
			slowExceptions++
		}
		if strings.HasPrefix(strings.TrimPrefix(r.text, "@@"), "/") && len(r.pattern.Literal()) < 5 {
			slowUnindexed++
		}
	}
	if len(patterns) > 0 {
		l.slowIndex = matcher.NewPatternIndex(patterns)
	}
	entries := l.primary.entries() + l.except.entries() + len(l.slow)
	l.stats = Stats{
		Entries:        entries,
		RuleCount:      c.ruleCount,
		Exceptions:     l.except.entries() + slowExceptions,
		Duplicates:     max(added-l.primary.entries()-l.except.entries(), 0) + max(c.slowAdded-len(l.slow), 0),
		Invalid:        c.invalid + c.regexInvalid + c.rejected,
		Unsupported:    c.unsupported,
		Ignored:        c.ignored,
		Regexes:        l.primary.regex.Len() + l.except.regex.Len() + c.slowRegexes(),
		RegexUnindexed: l.primary.regex.Unindexed() + l.except.regex.Unindexed() + slowUnindexed,
		RegexDropped:   c.regexDropped,
		Bytes:          l.primary.bytes() + l.except.bytes(),
	}
	for _, r := range l.slow {
		l.stats.Bytes += len(r.text) + len(r.key) + 2048
	}
	l.unsupported = c.unsupportedRules
	if l.stats.Regexes > matcher.RegexWarnThreshold {
		l.warnings = append(l.warnings, fmt.Sprintf("%d regex rules (more than %d); %d of them have no indexable literal", l.stats.Regexes, matcher.RegexWarnThreshold, l.stats.RegexUnindexed))
	}
	if l.stats.RegexUnindexed > matcher.RegexUnindexedWarnThreshold {
		l.warnings = append(l.warnings, fmt.Sprintf("%d regex rules have no literal suitable for indexing (4 characters for fast rules, 5 for modifier rules) and can run for every query (more than %d slows lookups noticeably)", l.stats.RegexUnindexed, matcher.RegexUnindexedWarnThreshold))
	}
	if c.regexDropped > 0 {
		l.warnings = append(l.warnings, fmt.Sprintf("%d regex rules dropped by the per-list hard cap of %d", c.regexDropped, c.regexCap))
	}
	if c.unsupported > 0 {
		reasons := slices.Sorted(maps.Keys(c.unsupportedReasons))
		parts := make([]string, len(reasons))
		for i, r := range reasons {
			parts[i] = fmt.Sprintf("%s %d", r, c.unsupportedReasons[r])
		}
		l.warnings = append(l.warnings, fmt.Sprintf("%d rules use features not supported in this version (%s)", c.unsupported, strings.Join(parts, ", ")))
	}
}
