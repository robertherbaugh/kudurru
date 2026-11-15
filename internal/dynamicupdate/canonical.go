package dynamicupdate

import (
	"encoding/binary"
	"fmt"
	"reflect"

	"github.com/miekg/dns"
)

// CanonicalName compares DNS names by label bytes, including escaped octets,
// and lowercases only ASCII letters. Unicode RDATA is never case-folded.
func CanonicalName(name string) (string, error) {
	if name == "" || len(name) > MaxNameBytes || !dns.IsFqdn(name) {
		return "", fmt.Errorf("absolute DNS name required")
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return "", fmt.Errorf("invalid DNS name")
	}
	wire := make([]byte, 255)
	n, err := dns.PackDomainName(name, wire, 0, nil, false)
	if err != nil {
		return "", err
	}
	for i := 0; i < n; {
		size := int(wire[i])
		i++
		if i+size > n {
			return "", fmt.Errorf("invalid DNS label")
		}
		for j := i; j < i+size; j++ {
			if wire[j] >= 'A' && wire[j] <= 'Z' {
				wire[j] += 'a' - 'A'
			}
		}
		i += size
	}
	canonical, _, err := dns.UnpackDomainName(wire[:n], 0)
	return canonical, err
}

func canonicalFields(v reflect.Value) error {
	for i := range v.NumField() {
		field := v.Field(i)
		tag := v.Type().Field(i).Tag.Get("dns")
		if field.Kind() == reflect.Struct {
			if err := canonicalFields(field); err != nil {
				return err
			}
		} else if tag == "domain-name" || tag == "cdomain-name" || tag == "ipsechost" || tag == "amtrelayhost" {
			set := func(f reflect.Value) error {
				// IPSECKEY/AMTRELAY may encode an empty hostname for other gateway types.
				if f.String() == "" {
					return nil
				}
				name, err := CanonicalName(f.String())
				if err != nil {
					return err
				}
				f.SetString(name)
				return nil
			}
			switch field.Kind() {
			case reflect.String:
				if err := set(field); err != nil {
					return err
				}
			case reflect.Slice:
				for j := range field.Len() {
					if err := set(field.Index(j)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Wire packs one uncompressed RR with a finite allocation. Header.Rdlength
// is not trusted: the packed bytes determine the actual RDATA length.
func Wire(rr dns.RR) ([]byte, error) {
	if rr == nil || reflect.ValueOf(rr).IsNil() {
		return nil, fmt.Errorf("nil RR")
	}
	size := dns.Len(rr)
	if size < 0 || size > 65535 {
		return nil, fmt.Errorf("oversized RR")
	}
	wire := make([]byte, size+16)
	n, err := dns.PackRR(Copy(rr), wire, 0, nil, false)
	if err != nil {
		return nil, err
	}
	if n > 65535 {
		return nil, fmt.Errorf("oversized RR")
	}
	return wire[:n], nil
}

// ValidateData also checks concrete RDATA framing, including opaque input for
// a known type. Empty RDATA is legal for unknown types, NULL and APL only.
func ValidateData(rr dns.RR) error {
	wire, err := Wire(rr)
	if err != nil {
		return err
	}
	n, err := rdataLength(wire)
	if err != nil {
		return err
	}
	typ := rr.Header().Rrtype
	if n == 0 {
		if _, known := dns.TypeToRR[typ]; known && typ != dns.TypeNULL && typ != dns.TypeAPL {
			return fmt.Errorf("missing RDATA")
		}
	}
	if typ == 11 && n < 5 {
		return fmt.Errorf("short WKS data")
	}
	_, end, err := dns.UnpackRR(wire, 0)
	if err != nil || end != len(wire) {
		return fmt.Errorf("invalid RDATA framing")
	}
	return nil
}

// Identity excludes TTL and class (the caller enforces zone class IN),
// canonicalizes embedded domain names and preserves opaque/text RDATA bytes.
func Identity(rr dns.RR) (string, error) {
	wire, err := Wire(rr)
	if err != nil {
		return "", err
	}
	cp := Copy(rr)
	// Decode known nonempty opaque data so embedded names compare exactly as
	// they do after a real listener unpack. Unknown types remain byte-exact.
	if n, e := rdataLength(wire); e == nil && n > 0 {
		decoded, end, e := dns.UnpackRR(wire, 0)
		if e != nil || end != len(wire) {
			return "", fmt.Errorf("invalid RDATA framing")
		}
		cp = decoded
	}
	name, err := CanonicalName(cp.Header().Name)
	if err != nil {
		return "", err
	}
	cp.Header().Name = name
	cp.Header().Class = dns.ClassINET
	cp.Header().Ttl = 0
	if err := canonicalFields(reflect.ValueOf(cp).Elem()); err != nil {
		return "", err
	}
	wire, err = Wire(cp)
	return string(wire), err
}

func rdataLength(wire []byte) (int, error) {
	_, off, err := dns.UnpackDomainName(wire, 0)
	if err != nil || off+10 > len(wire) {
		return 0, fmt.Errorf("malformed RR framing")
	}
	n := int(binary.BigEndian.Uint16(wire[off+8 : off+10]))
	if off+10+n != len(wire) {
		return 0, fmt.Errorf("malformed RDATA framing")
	}
	return n, nil
}

func dataType(t uint16) bool {
	return t != 0 && t != 65535 && t != dns.TypeOPT && (t < dns.TypeTKEY || t > dns.TypeANY)
}

// Copy preserves RFC 2136 empty-RDATA meta records as well as ordinary RRs.
// miekg/dns Copy intentionally returns nil for standalone RR_Header values.
func Copy(rr dns.RR) dns.RR {
	if h, ok := rr.(*dns.RR_Header); ok {
		cp := *h
		return &cp
	}
	return dns.Copy(rr)
}
