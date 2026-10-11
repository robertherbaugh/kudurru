package querylog

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"sync"

	"k8s.io/utils/clock"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// Mode is the querylog.privacy level, applied at ingest and irreversible
// (docs/architecture.md §11.2, D59).
type Mode uint8

// Privacy modes, from most to least detail.
const (
	// PrivacyFull stores everything, including the answer summary.
	PrivacyFull Mode = iota
	// PrivacyAnonymizeClient truncates client addresses to /24 (IPv4) and
	// /48 (IPv6), or replaces them with a daily HMAC pseudonym, and drops
	// client names and ECS.
	PrivacyAnonymizeClient
	// PrivacyHideDomains replaces every name with "hidden".
	PrivacyHideDomains
	// PrivacyHideDomainsClients hides names and replaces the client with
	// 0.0.0.0.
	PrivacyHideDomainsClients
	// PrivacyNothing stores no fact rows and no per-client or per-domain
	// rollups; only the per-minute totals survive.
	PrivacyNothing
)

var modeNames = [...]string{"full", "anonymize_client", "hide_domains", "hide_domains_clients", "nothing"}

// String returns the config spelling of the mode.
func (m Mode) String() string {
	if int(m) < len(modeNames) {
		return modeNames[m]
	}
	return "unknown"
}

// ParseMode parses a querylog.privacy value.
func ParseMode(s string) (Mode, error) {
	for i, n := range modeNames {
		if n == s {
			return Mode(i), nil
		}
	}
	return 0, fmt.Errorf("querylog: unknown privacy mode %q (%s)", s, strings.Join(modeNames[:], ", "))
}

// StoresFacts reports whether the mode keeps per-query rows.
func (m Mode) StoresFacts() bool { return m != PrivacyNothing }

// HiddenName is the domain stored under hide_domains and stricter modes.
const HiddenName = "hidden"

// PseudonymPrefix starts every daily client pseudonym.
const PseudonymPrefix = "anon-"

// Anonymization methods for PrivacyAnonymizeClient.
const (
	AnonymizePrefix    = "prefix"
	AnonymizePseudonym = "pseudonym"
)

// Privacy is the ingest filter: the exclude lists and the privacy
// transform, applied once per event by the collector goroutine. It is not
// safe for concurrent Apply calls from several goroutines except through
// the pseudonym key, which is locked.
type Privacy struct {
	mode      Mode
	pseudonym bool
	clock     clock.PassiveClock

	excludeExact  map[string]struct{}
	excludeSuffix []string // ".example.com" for "*.example.com"
	excludeRegex  []*regexp.Regexp
	excludeNets   []netip.Prefix
	excludeNames  map[string]struct{}
	sudn          bool

	mu       sync.Mutex
	keyDay   int64
	dailyKey []byte
}

// PrivacyOptions configures NewPrivacy.
type PrivacyOptions struct {
	Mode Mode
	// Pseudonym selects the daily-HMAC pseudonym instead of prefix
	// truncation under PrivacyAnonymizeClient. The key is random, held only
	// in memory and rotated at UTC midnight, so pseudonyms cannot be
	// reversed from the database (they change across a restart).
	Pseudonym bool
	// ExcludeDomains: exact names, "*.suffix" wildcards (the suffix and
	// every name below it) or "/regex/".
	ExcludeDomains []string
	// ExcludeClients: IP addresses, CIDR prefixes or client names.
	ExcludeClients []string
	// ExcludeSUDN drops special-use and private reverse names (RFC 6761,
	// RFC 6762, RFC 8375, RFC 7686, RFC 1918/4193/4291 reverse zones).
	ExcludeSUDN bool
	// Clock drives the pseudonym key rotation; nil means the wall clock.
	Clock clock.PassiveClock
}

// NewPrivacy compiles the exclude lists.
func NewPrivacy(o PrivacyOptions) (*Privacy, error) {
	p := &Privacy{
		mode:         o.Mode,
		pseudonym:    o.Pseudonym,
		clock:        o.Clock,
		excludeExact: map[string]struct{}{},
		excludeNames: map[string]struct{}{},
		sudn:         o.ExcludeSUDN,
	}
	if p.clock == nil {
		p.clock = clock.RealClock{}
	}
	for _, d := range o.ExcludeDomains {
		d = strings.TrimSpace(d)
		switch {
		case d == "":
		case len(d) > 2 && strings.HasPrefix(d, "/") && strings.HasSuffix(d, "/"):
			re, err := regexp.Compile(d[1 : len(d)-1])
			if err != nil {
				return nil, fmt.Errorf("querylog: exclude.domains %q: %w", d, err)
			}
			p.excludeRegex = append(p.excludeRegex, re)
		case strings.HasPrefix(d, "*."):
			s := canonical(d[2:])
			p.excludeExact[s] = struct{}{}
			p.excludeSuffix = append(p.excludeSuffix, "."+s)
		default:
			p.excludeExact[canonical(d)] = struct{}{}
		}
	}
	for _, c := range o.ExcludeClients {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if pfx, err := netip.ParsePrefix(c); err == nil {
			p.excludeNets = append(p.excludeNets, pfx.Masked())
		} else if a, err := netip.ParseAddr(c); err == nil {
			a = a.Unmap()
			p.excludeNets = append(p.excludeNets, netip.PrefixFrom(a, a.BitLen()))
		} else {
			p.excludeNames[strings.ToLower(c)] = struct{}{}
		}
	}
	return p, nil
}

// PrivacyFrom builds the ingest filter from the querylog settings.
func PrivacyFrom(c config.Querylog) (*Privacy, error) {
	mode, err := ParseMode(c.Privacy)
	if err != nil {
		return nil, err
	}
	return NewPrivacy(PrivacyOptions{
		Mode:           mode,
		Pseudonym:      c.Anonymize == AnonymizePseudonym,
		ExcludeDomains: c.Exclude.Domains,
		ExcludeClients: c.Exclude.Clients,
		ExcludeSUDN:    c.Exclude.SUDN,
	})
}

// Mode returns the privacy mode.
func (p *Privacy) Mode() Mode { return p.mode }

// canonical lower-cases and strips the trailing dot.
func canonical(name string) string { return strings.TrimSuffix(strings.ToLower(name), ".") }

// Excluded reports whether the event matches an exclude list. It reads the
// event before the transform (real client, real name).
func (p *Privacy) Excluded(ev *Event) bool {
	name := canonical(ev.Name)
	if _, ok := p.excludeExact[name]; ok {
		return true
	}
	for _, s := range p.excludeSuffix {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	for _, re := range p.excludeRegex {
		if re.MatchString(name) {
			return true
		}
	}
	if p.sudn && IsSpecialUse(name) {
		return true
	}
	if len(p.excludeNets) > 0 && ev.Client.IsValid() {
		a := ev.Client.Unmap()
		for _, n := range p.excludeNets {
			if n.Contains(a) {
				return true
			}
		}
	}
	if len(p.excludeNames) > 0 && ev.ClientName != "" {
		if _, ok := p.excludeNames[strings.ToLower(ev.ClientName)]; ok {
			return true
		}
	}
	return false
}

// Apply runs the exclude lists and then the privacy transform on ev in
// place. It returns false when the event must not be logged at all.
func (p *Privacy) Apply(ev *Event) bool {
	if p.Excluded(ev) {
		return false
	}
	if p.mode != PrivacyFull {
		ev.Answer = "" // answer summary only in full (D59)
	}
	switch p.mode {
	case PrivacyFull:
	case PrivacyAnonymizeClient:
		p.anonymizeClient(ev)
	case PrivacyHideDomains:
		hideDomains(ev)
	case PrivacyHideDomainsClients, PrivacyNothing:
		hideDomains(ev)
		hideClient(ev)
	}
	return true
}

func (p *Privacy) anonymizeClient(ev *Event) {
	ev.ECS = netip.Prefix{}
	ev.Flags.ECS = false
	if p.pseudonym {
		name := ""
		if ev.Client.IsValid() {
			name = p.pseudonymFor(ev.Client.Unmap())
		}
		ev.Client = netip.Addr{}
		ev.ClientName = name
		return
	}
	ev.ClientName = ""
	if !ev.Client.IsValid() {
		return
	}
	a := ev.Client.Unmap()
	bits := 24
	if a.Is6() {
		bits = 48
	}
	ev.Client = netip.PrefixFrom(a, bits).Masked().Addr()
}

func hideDomains(ev *Event) {
	ev.Name = HiddenName
	if ev.CNAME != "" {
		ev.CNAME = HiddenName
	}
	ev.Rule = "" // rule text names the domain
	ev.Answer = ""
}

func hideClient(ev *Event) {
	ev.Client = netip.IPv4Unspecified()
	ev.ClientName = ""
	ev.ECS = netip.Prefix{}
	ev.Flags.ECS = false
}

// pseudonymFor returns "anon-" and 12 hex digits of HMAC-SHA256(daily key,
// address); the key rotates at UTC midnight.
func (p *Privacy) pseudonymFor(a netip.Addr) string {
	day := p.clock.Now().UTC().Unix() / 86400
	p.mu.Lock()
	if p.dailyKey == nil || day != p.keyDay {
		k := make([]byte, 32)
		_, _ = rand.Read(k) // crypto/rand.Read never fails (Go 1.24+)
		p.dailyKey, p.keyDay = k, day
	}
	mac := hmac.New(sha256.New, p.dailyKey)
	p.mu.Unlock()
	b := a.As16()
	mac.Write(b[:])
	return PseudonymPrefix + hex.EncodeToString(mac.Sum(nil)[:6])
}

// specialUse are the names querylog.exclude.sudn drops: RFC 6761
// (localhost, invalid, test), RFC 6762 (local), RFC 8375 (home.arpa),
// RFC 7686 (onion), RFC 9476 (alt), loopback/link-local/private reverse
// zones (RFC 1918, RFC 3927, RFC 4193, RFC 4291).
var specialUse = func() []string {
	s := []string{
		"localhost", "invalid", "test", "local", "home.arpa", "onion", "alt",
		"10.in-addr.arpa", "127.in-addr.arpa", "254.169.in-addr.arpa", "168.192.in-addr.arpa",
		"0.in-addr.arpa", "255.255.255.255.in-addr.arpa",
		"c.f.ip6.arpa", "d.f.ip6.arpa", "8.e.f.ip6.arpa", "9.e.f.ip6.arpa", "a.e.f.ip6.arpa", "b.e.f.ip6.arpa",
		"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa",
	}
	for i := 16; i <= 31; i++ {
		s = append(s, fmt.Sprintf("%d.172.in-addr.arpa", i))
	}
	return s
}()

// IsSpecialUse reports whether name (canonical, no trailing dot) is or lies
// below a special-use or private reverse name.
func IsSpecialUse(name string) bool {
	name = canonical(name)
	for _, s := range specialUse {
		if name == s || strings.HasSuffix(name, "."+s) {
			return true
		}
	}
	return false
}
