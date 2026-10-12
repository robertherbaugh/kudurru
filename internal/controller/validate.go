package controller

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// EndpointValidator reports why an endpoint cannot be served. It is the
// seam through which internal/zone's presentation-format parser (which
// owns the miekg/dns types, §2) replaces ValidateEndpoint.
type EndpointValidator func(ep v1alpha1.Endpoint) error

// ValidateEndpoint is the default EndpointValidator: it checks the owner
// name and the presentation format of the common record types without
// miekg/dns. Types it does not know only need non-empty targets; the zone
// parser is authoritative for them.
func ValidateEndpoint(ep v1alpha1.Endpoint) error {
	if err := validName(ep.DNSName, true); err != nil {
		return fmt.Errorf("dnsName %q: %w", ep.DNSName, err)
	}
	if ep.RecordTTL < 0 || ep.RecordTTL > math.MaxInt32 {
		return fmt.Errorf("%s %s: recordTTL %d out of range", ep.DNSName, ep.RecordType, ep.RecordTTL)
	}
	if len(ep.Targets) == 0 {
		return fmt.Errorf("%s %s: no targets", ep.DNSName, ep.RecordType)
	}
	typ := strings.ToUpper(ep.RecordType)
	if (typ == "CNAME" || typ == "DNAME") && len(ep.Targets) > 1 {
		return fmt.Errorf("%s %s: exactly one target allowed, got %d", ep.DNSName, typ, len(ep.Targets))
	}
	for _, t := range ep.Targets {
		if err := validTarget(typ, t); err != nil {
			return fmt.Errorf("%s %s target %q: %w", ep.DNSName, typ, t, err)
		}
	}
	return nil
}

// validTarget checks one target in presentation format.
func validTarget(typ, t string) error {
	if strings.TrimSpace(t) == "" {
		return errors.New("empty")
	}
	switch typ {
	case "A":
		a, err := netip.ParseAddr(t)
		if err != nil || !a.Is4() {
			return errors.New("not an IPv4 address")
		}
	case "AAAA":
		a, err := netip.ParseAddr(t)
		if err != nil || !a.Is6() || a.Zone() != "" {
			return errors.New("not an IPv6 address")
		}
	case "CNAME", "DNAME", "NS", "PTR":
		return validName(t, false)
	case "MX":
		f := strings.Fields(t)
		if len(f) != 2 {
			return errors.New(`want "preference exchange"`)
		}
		if err := uintField(f[0], 16, "preference"); err != nil {
			return err
		}
		return rootOrName(f[1])
	case "SRV":
		f := strings.Fields(t)
		if len(f) != 4 {
			return errors.New(`want "priority weight port target"`)
		}
		for i, n := range []string{"priority", "weight", "port"} {
			if err := uintField(f[i], 16, n); err != nil {
				return err
			}
		}
		return rootOrName(f[3])
	case "CAA":
		f := strings.Fields(t)
		if len(f) < 3 {
			return errors.New(`want "flags tag value"`)
		}
		if err := uintField(f[0], 8, "flags"); err != nil {
			return err
		}
		for _, r := range f[1] {
			if !isAlnum(r) {
				return fmt.Errorf("tag %q is not alphanumeric", f[1])
			}
		}
	}
	return nil
}

func uintField(s string, bits int, name string) error {
	if _, err := strconv.ParseUint(s, 10, bits); err != nil {
		return fmt.Errorf("%s %q is not a %d-bit unsigned integer", name, s, bits)
	}
	return nil
}

func rootOrName(s string) error {
	if s == "." {
		return nil
	}
	return validName(s, false)
}

// validName checks a domain name with or without the trailing dot. Owner
// names may start with a "*" wildcard label; underscores are allowed
// everywhere (service labels, DKIM selectors).
func validName(s string, owner bool) error {
	n := strings.TrimSuffix(s, ".")
	if n == "" {
		return errors.New("empty name")
	}
	if len(n) > 253 {
		return errors.New("longer than 253 characters")
	}
	for i, label := range strings.Split(n, ".") {
		switch {
		case label == "":
			return errors.New("empty label")
		case len(label) > 63:
			return fmt.Errorf("label %q longer than 63 characters", label)
		case label == "*" && owner && i == 0:
			continue
		}
		for _, r := range label {
			if !isAlnum(r) && r != '-' && r != '_' {
				return fmt.Errorf("label %q has invalid character %q", label, r)
			}
		}
	}
	return nil
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// canonicalName lower-cases a name and makes it fully qualified.
func canonicalName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// inZone reports whether the canonical name is the origin or below it.
func inZone(name, origin string) bool {
	if origin == "." {
		return true
	}
	return name == origin || strings.HasSuffix(name, "."+origin)
}
