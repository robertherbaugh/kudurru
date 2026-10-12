package querylog

import (
	"strconv"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// The read API speaks symbolic names. These tables keep miekg/dns out of
// the query-log packages (docs/architecture.md §2): they cover the types
// and rcodes a resolver log shows; anything else is TYPEnnn / RCODEnnn
// (RFC 3597 style).

var qtypeNames = map[uint16]string{
	1: "A", 2: "NS", 5: "CNAME", 6: "SOA", 12: "PTR", 13: "HINFO", 15: "MX", 16: "TXT",
	17: "RP", 18: "AFSDB", 24: "SIG", 25: "KEY", 28: "AAAA", 29: "LOC", 33: "SRV", 35: "NAPTR",
	36: "KX", 37: "CERT", 39: "DNAME", 41: "OPT", 42: "APL", 43: "DS", 44: "SSHFP", 45: "IPSECKEY",
	46: "RRSIG", 47: "NSEC", 48: "DNSKEY", 49: "DHCID", 50: "NSEC3", 51: "NSEC3PARAM", 52: "TLSA",
	53: "SMIMEA", 55: "HIP", 59: "CDS", 60: "CDNSKEY", 61: "OPENPGPKEY", 62: "CSYNC", 63: "ZONEMD",
	64: "SVCB", 65: "HTTPS", 99: "SPF", 108: "EUI48", 109: "EUI64", 249: "TKEY", 250: "TSIG",
	251: "IXFR", 252: "AXFR", 255: "ANY", 256: "URI", 257: "CAA",
}

var qtypeValues = func() map[string]uint16 {
	m := make(map[string]uint16, len(qtypeNames))
	for v, n := range qtypeNames {
		m[n] = v
	}
	return m
}()

// QTypeName returns the mnemonic of a query type ("A", "HTTPS", "TYPE65280").
func QTypeName(t uint16) string {
	if n, ok := qtypeNames[t]; ok {
		return n
	}
	return "TYPE" + strconv.Itoa(int(t))
}

// ParseQType parses a mnemonic or TYPEnnn, case-insensitively.
func ParseQType(s string) (uint16, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if v, ok := qtypeValues[s]; ok {
		return v, true
	}
	if rest, ok := strings.CutPrefix(s, "TYPE"); ok {
		if v, err := strconv.ParseUint(rest, 10, 16); err == nil {
			return uint16(v), true
		}
	}
	return 0, false
}

var rcodeNames = map[int]string{
	0: "NOERROR", 1: "FORMERR", 2: "SERVFAIL", 3: "NXDOMAIN", 4: "NOTIMP", 5: "REFUSED",
	6: "YXDOMAIN", 7: "YXRRSET", 8: "NXRRSET", 9: "NOTAUTH", 10: "NOTZONE", 16: "BADSIG",
	17: "BADKEY", 18: "BADTIME", 19: "BADMODE", 20: "BADNAME", 21: "BADALG", 22: "BADTRUNC", 23: "BADCOOKIE",
}

// RcodeName returns the mnemonic of a response code ("NXDOMAIN", "RCODE99").
func RcodeName(rc int) string {
	if n, ok := rcodeNames[rc]; ok {
		return n
	}
	return "RCODE" + strconv.Itoa(rc)
}

// ParseStatus parses a status name of the taxonomy ("blocked_list").
func ParseStatus(s string) (pipeline.Status, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for st := pipeline.Status(0); st < 32; st++ {
		if st.String() == s && (s != "unknown" || st == pipeline.StatusUnknown) {
			return st, true
		}
	}
	return 0, false
}
