package rpz

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// RPZ addresses reverse complete bytes/16-bit words, not IPv6 nibbles.
func addressPrefix(labels []string) (netip.Prefix, error) {
	if len(labels) < 2 || strings.HasPrefix(labels[0], "+") {
		return netip.Prefix{}, fmt.Errorf("rpz: missing address prefix")
	}
	bits, err := strconv.ParseUint(labels[0], 10, 8)
	if err != nil || bits == 0 {
		return netip.Prefix{}, fmt.Errorf("rpz: invalid address prefix length")
	}
	labels = labels[1:]
	var addr netip.Addr
	if len(labels) == 4 && !containsZeroRun(labels) {
		var bytes [4]byte
		for i, label := range labels {
			if strings.HasPrefix(label, "+") {
				return netip.Prefix{}, fmt.Errorf("rpz: invalid IPv4 byte")
			}
			n, err := strconv.ParseUint(label, 10, 8)
			if err != nil {
				return netip.Prefix{}, fmt.Errorf("rpz: invalid IPv4 byte")
			}
			bytes[3-i] = byte(n)
		}
		if bits > 32 {
			return netip.Prefix{}, fmt.Errorf("rpz: invalid IPv4 prefix length")
		}
		addr = netip.AddrFrom4(bytes)
	} else {
		words := make([]uint16, 0, 8)
		zeroRun := -1
		for _, label := range labels {
			if label == "zz" {
				if zeroRun >= 0 {
					return netip.Prefix{}, fmt.Errorf("rpz: multiple IPv6 zero runs")
				}
				zeroRun = len(words)
				continue
			}
			if len(label) < 1 || len(label) > 4 || strings.HasPrefix(label, "+") {
				return netip.Prefix{}, fmt.Errorf("rpz: invalid IPv6 word")
			}
			n, err := strconv.ParseUint(label, 16, 16)
			if err != nil {
				return netip.Prefix{}, fmt.Errorf("rpz: invalid IPv6 word")
			}
			words = append(words, uint16(n))
		}
		if zeroRun >= 0 {
			missing := 8 - len(words)
			if missing <= 0 {
				return netip.Prefix{}, fmt.Errorf("rpz: empty IPv6 zero run")
			}
			words = append(words[:zeroRun], append(make([]uint16, missing), words[zeroRun:]...)...)
		}
		if len(words) != 8 || bits > 128 {
			return netip.Prefix{}, fmt.Errorf("rpz: invalid IPv6 address/prefix length")
		}
		var bytes [16]byte
		for i, word := range words {
			bytes[(7-i)*2], bytes[(7-i)*2+1] = byte(word>>8), byte(word&0xff)
		}
		addr = netip.AddrFrom16(bytes)
	}
	prefix := netip.PrefixFrom(addr, int(bits))
	if prefix != prefix.Masked() {
		return netip.Prefix{}, fmt.Errorf("rpz: address prefix has host bits set")
	}
	return prefix, nil
}

func containsZeroRun(labels []string) bool {
	for _, s := range labels {
		if s == "zz" {
			return true
		}
	}
	return false
}
