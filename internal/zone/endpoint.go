package zone

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// SupportedTypes lists the record types an Endpoint may carry by mnemonic
// (docs/architecture.md §4.1). Any other type is written as TYPEnnn with
// RFC 3597 rdata ("\# <len> <hex>").
var SupportedTypes = []string{
	"A", "AAAA", "CAA", "CNAME", "DNAME", "HTTPS", "MX", "NAPTR", "NS",
	"PTR", "SRV", "SSHFP", "SVCB", "TLSA", "TXT",
}

var supportedTypes = func() map[string]uint16 {
	m := make(map[string]uint16, len(SupportedTypes))
	for _, s := range SupportedTypes {
		m[s] = dns.StringToType[s]
	}
	return m
}()

// forbiddenType reports whether type t may never appear in Endpoint data,
// not even as TYPEnnn: the SOA is generated from the DNSZone, meta and
// query-only types are not data, and DNSSEC records belong to the v1.0
// signer.
func forbiddenType(t uint16) bool {
	switch t {
	case 0, dns.TypeSOA, dns.TypeOPT, dns.TypeTSIG, dns.TypeTKEY,
		dns.TypeAXFR, dns.TypeIXFR, dns.TypeMAILA, dns.TypeMAILB, dns.TypeANY,
		dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeNSEC3PARAM,
		dns.TypeDNSKEY, dns.TypeCDS, dns.TypeCDNSKEY, dns.TypeNXT, dns.TypeSIG, dns.TypeKEY:
		return true
	}
	return false
}

// parseType resolves an Endpoint recordType to a type code.
func parseType(s string) (uint16, error) {
	if t, ok := supportedTypes[s]; ok {
		return t, nil
	}
	if rest, ok := strings.CutPrefix(s, "TYPE"); ok && rest != "" {
		n, err := strconv.ParseUint(rest, 10, 16)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("TYPEnnn needs a number between 1 and 65535")
		}
		if forbiddenType(uint16(n)) {
			return 0, fmt.Errorf("type %s cannot be served from records", typeString(uint16(n)))
		}
		return uint16(n), nil
	}
	return 0, fmt.Errorf("unsupported record type; use one of %s or TYPEnnn (RFC 3597)", strings.Join(SupportedTypes, " "))
}

// OwnerName resolves an Endpoint dnsName against the zone origin. Names are
// absolute whether or not they end in a dot (the external-dns convention);
// "@" stands for the apex. Presentation escapes (\. \032) are allowed;
// unescaped whitespace and zone-file syntax characters are not, and
// neither is "$" anywhere in the name (a "$" leading an exported line
// would read as a directive). The result is the canonical spelling
// (canonicalName): lowercased, absolute and at or below origin.
func OwnerName(origin, dnsName string) (string, error) {
	name := strings.TrimSpace(dnsName)
	switch name {
	case "":
		return "", fmt.Errorf("must not be empty")
	case "@":
		return canonicalName(origin), nil
	}
	if hasUnescaped(name, " \t\r\n;()\"") {
		return "", fmt.Errorf("must be a domain name without whitespace or zone-file syntax")
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return "", errBadName
	}
	key, err := canonicalKey(name)
	if err != nil {
		return "", err
	}
	if strings.IndexByte(key, '$') >= 0 {
		return "", fmt.Errorf("must not contain $")
	}
	okey, err := canonicalKey(origin)
	if err != nil {
		return "", err
	}
	if !isKeyPrefix(key, okey) {
		return "", fmt.Errorf("%s is outside zone %s", nameFromKey(key), nameFromKey(okey))
	}
	return nameFromKey(key), nil
}

// hasUnescaped reports whether s contains a byte of set that is not part of
// a presentation-format escape.
func hasUnescaped(s, set string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
				i += 3
			} else {
				i++
			}
			continue
		}
		if strings.IndexByte(set, s[i]) >= 0 {
			return true
		}
	}
	return false
}

// placeholderOwner is the owner used while parsing rdata, replaced by the
// real owner afterwards so that a hostile dnsName can never reach the
// zone-file parser.
const placeholderOwner = "kudurru.invalid."

// ParseTarget parses one presentation-format target of type rrtype for
// owner. Names inside the rdata are absolute whether or not they end in a
// dot, matching Endpoint.dnsName and external-dns. TXT targets that do not
// start with a double quote are taken as one raw string (external-dns
// writes TXT values unquoted) and quoted here; quoted targets are parsed as
// presentation format, so `"a" "b"` is two character strings.
func ParseTarget(owner string, rrtype uint16, ttl uint32, target string) (dns.RR, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return nil, fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(t, "\r\n") {
		return nil, fmt.Errorf("must be a single line")
	}
	if rrtype == dns.TypeTXT && !strings.HasPrefix(t, `"`) {
		t = quoteTXT(target)
	}
	line := placeholderOwner + " " + strconv.FormatUint(uint64(ttl), 10) + " IN " + typeString(rrtype) + " " + t
	rr, err := dns.NewRR(line)
	if err != nil {
		return nil, fmt.Errorf("does not parse as %s rdata: %s", typeString(rrtype), parseErrText(err))
	}
	if rr == nil {
		return nil, fmt.Errorf("does not parse as %s rdata", typeString(rrtype))
	}
	if got := rr.Header().Rrtype; got != rrtype {
		return nil, fmt.Errorf("parsed as %s, want %s", typeString(got), typeString(rrtype))
	}
	if forbiddenType(rr.Header().Rrtype) {
		return nil, fmt.Errorf("type %s cannot be served from records", typeString(rrtype))
	}
	if err := checkRdata(rr); err != nil {
		return nil, err
	}
	rr.Header().Name = owner
	return rr, nil
}

// checkRdata rejects records that parse but cannot be served: empty rdata
// (miekg accepts "A" with nothing after it), root targets where the type
// needs a host, and anything that does not pack.
func checkRdata(rr dns.RR) error {
	c := dns.Copy(rr)
	buf := make([]byte, dns.Len(c)+64)
	off, err := dns.PackRR(c, buf, 0, nil, false)
	if err != nil {
		return fmt.Errorf("invalid rdata: %v", err)
	}
	if _, _, err := dns.UnpackRR(buf[:off], 0); err != nil {
		return fmt.Errorf("invalid rdata: %v", err)
	}
	if c.Header().Rdlength == 0 {
		if _, unknown := rr.(*dns.RFC3597); !unknown {
			return fmt.Errorf("rdata must not be empty")
		}
	}
	switch r := rr.(type) {
	case *dns.CNAME:
		if r.Target == "." {
			return fmt.Errorf("a CNAME target cannot be the root")
		}
	case *dns.DNAME:
		if r.Target == "." {
			return fmt.Errorf("a DNAME target cannot be the root")
		}
	case *dns.NS:
		if r.Ns == "." {
			return fmt.Errorf("an NS target cannot be the root")
		}
	case *dns.PTR:
		if r.Ptr == "." {
			return fmt.Errorf("a PTR target cannot be the root")
		}
	}
	return nil
}

// quoteTXT turns a raw string into one presentation-format character
// string; miekg splits strings longer than 255 octets.
func quoteTXT(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// parseErrText strips the placeholder owner and line positions from a
// miekg parse error so the message is about the target alone.
func parseErrText(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "dns: ")
	if i := strings.Index(s, " at line: "); i >= 0 {
		s = s[:i]
	}
	return s
}

// TTL resolves an Endpoint recordTTL: zero means the zone default.
func TTL(recordTTL int64, defaultTTL uint32) (uint32, error) {
	switch {
	case recordTTL < 0 || recordTTL > math.MaxInt32:
		return 0, fmt.Errorf("must be between 0 and %d seconds", math.MaxInt32)
	case recordTTL == 0:
		return defaultTTL, nil
	}
	return uint32(recordTTL), nil
}

// ParseEndpoint turns one Endpoint into records for the zone with the given
// origin and default TTL. Errors carry field paths below path (for example
// spec.endpoints[3].targets[1]) so the controller can surface them in a
// condition. Records are returned for every target that parsed even when
// others failed; callers decide whether a partial endpoint is served (the
// Builder is all-or-nothing per endpoint).
func ParseEndpoint(origin string, defaultTTL uint32, ep *v1alpha1.Endpoint, path *field.Path) ([]dns.RR, field.ErrorList) {
	var errs field.ErrorList
	owner, err := OwnerName(origin, ep.DNSName)
	if err != nil {
		errs = append(errs, field.Invalid(path.Child("dnsName"), ep.DNSName, err.Error()))
	}
	rrtype, err := parseType(ep.RecordType)
	if err != nil {
		errs = append(errs, field.NotSupported(path.Child("recordType"), ep.RecordType, append(SupportedTypes, "TYPEnnn")))
	}
	ttl, err := TTL(ep.RecordTTL, defaultTTL)
	if err != nil {
		errs = append(errs, field.Invalid(path.Child("recordTTL"), ep.RecordTTL, err.Error()))
	}
	if len(ep.Targets) == 0 {
		errs = append(errs, field.Required(path.Child("targets"), "at least one target is required"))
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if (rrtype == dns.TypeCNAME || rrtype == dns.TypeDNAME) && len(ep.Targets) > 1 {
		return nil, field.ErrorList{field.TooMany(path.Child("targets"), len(ep.Targets), 1)}
	}
	rrs := make([]dns.RR, 0, len(ep.Targets))
	for i, t := range ep.Targets {
		rr, err := ParseTarget(owner, rrtype, ttl, t)
		if err != nil {
			errs = append(errs, field.Invalid(path.Child("targets").Index(i), t, err.Error()))
			continue
		}
		rrs = append(rrs, rr)
	}
	return rrs, errs
}
