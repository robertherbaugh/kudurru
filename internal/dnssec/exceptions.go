package dnssec

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/miekg/dns"
)

// Exceptions is a bounded immutable plan shared by forwarded and recursive
// validation. Expiry instants are copied, never converted into renewable TTLs.
type Exceptions struct {
	ntas []NTA
	id   [32]byte
}

// NewExceptions validates and copies temporary operator exceptions. Expired
// entries remain valid configuration, but Match never activates them.
func NewExceptions(ntas []NTA, now time.Time) (*Exceptions, error) {
	if len(ntas) > 128 {
		return nil, fmt.Errorf("dnssec: at most 128 negative trust anchors are supported")
	}
	e := &Exceptions{}
	seen := map[string]bool{}
	for _, n := range ntas {
		if n.Domain == "" || strings.IndexFunc(n.Domain, unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("dnssec: NTA domain is required")
		}
		n.Domain = dns.CanonicalName(n.Domain)
		if _, ok := dns.IsDomainName(n.Domain); !ok || strings.TrimSpace(n.Reason) == "" || len(n.Reason) > 256 || n.ExpiresAt.IsZero() || n.ExpiresAt.After(now.Add(7*24*time.Hour)) || seen[n.Domain] {
			return nil, fmt.Errorf("dnssec: NTA requires a unique domain, reason, and expiry within 7 days")
		}
		seen[n.Domain] = true
		e.ntas = append(e.ntas, n)
	}
	sort.Slice(e.ntas, func(i, j int) bool { return e.ntas[i].Domain < e.ntas[j].Domain })
	b, _ := json.Marshal(e.ntas)
	e.id = sha256.Sum256(b)
	return e, nil
}

// Match returns the most specific active exception's absolute deadline.
func (e *Exceptions) Match(name string, now time.Time) (time.Time, bool) {
	var until time.Time
	best := -1
	name = dns.CanonicalName(name)
	for _, n := range e.ntas {
		if now.Before(n.ExpiresAt) && dns.IsSubDomain(n.Domain, name) {
			labels := dns.CountLabel(n.Domain)
			if labels > best {
				until, best = n.ExpiresAt, labels
			}
		}
	}
	return until, best >= 0
}

// Context returns time-dependent cache identity and the next absolute expiry.
// Conservatively invalidate the whole native cache when any exception expires.
func (e *Exceptions) Context(now time.Time) ([32]byte, time.Time) {
	return e.context(e.id, now)
}

func (e *Exceptions) context(base [32]byte, now time.Time) ([32]byte, time.Time) {
	var b strings.Builder
	b.Write(base[:])
	var until time.Time
	for _, n := range e.ntas {
		if now.Before(n.ExpiresAt) {
			b.WriteString(n.Domain)
			b.WriteByte(0)
			if until.IsZero() || n.ExpiresAt.Before(until) {
				until = n.ExpiresAt
			}
		}
	}
	return sha256.Sum256([]byte(b.String())), until
}
