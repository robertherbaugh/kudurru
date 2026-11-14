package cache

import (
	"encoding/binary"
	"net/netip"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Key identifies a cached response: query name (case-folded), type, class,
// the DO and CD bits, and the ECS subnet sent upstream. Build it with
// MakeKey; the encoding is compact and part of the Dump format.
type Key string

// Key flag bits.
const (
	flagDO     = 1 << 0
	flagCD     = 1 << 1
	flagECS4   = 1 << 2
	flagECS6   = 1 << 3
	flagScoped = 1 << 4
)

// MakeKey builds the key. name is folded to lower case (ASCII, as DNS
// names compare). ecs is the subnet sent upstream, or an invalid prefix.
func MakeKey(name string, qtype, qclass uint16, do, cd bool, ecs netip.Prefix) Key {
	n := 5 + len(name)
	if ecs.IsValid() {
		n += 1 + ecs.Addr().BitLen()/8
	}
	b := make([]byte, 5, n)
	binary.BigEndian.PutUint16(b[0:], qtype)
	binary.BigEndian.PutUint16(b[2:], qclass)
	var flags byte
	if do {
		flags |= flagDO
	}
	if cd {
		flags |= flagCD
	}
	if ecs.IsValid() {
		ecs = ecs.Masked()
		if ecs.Addr().Is4() {
			flags |= flagECS4
		} else {
			flags |= flagECS6
		}
		b = append(b, byte(ecs.Bits())) //nolint:gosec // 0..128
		b = append(b, ecs.Addr().AsSlice()...)
	}
	b[4] = flags
	for i := 0; i < len(name); i++ {
		c := name[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	return Key(b)
}

// KeyParts is a decoded Key.
type KeyParts struct {
	Name        string
	Type, Class uint16
	DO, CD      bool
	ECS         netip.Prefix
	Scope       pipeline.CacheScope
}

// MakeScopedKey binds a DNS cache key to the captured resolution incarnation.
func MakeScopedKey(scope pipeline.CacheScope, name string, qtype, qclass uint16, do, cd bool, ecs netip.Prefix) Key {
	base := MakeKey(name, qtype, qclass, do, cd, ecs)
	b := make([]byte, 45, len(base)+40)
	copy(b, base[:5])
	b[4] |= flagScoped
	copy(b[5:37], scope.ID[:])
	binary.BigEndian.PutUint64(b[37:45], scope.Epoch)
	b = append(b, base[5:]...)
	return Key(b)
}

// Parts decodes the key; ok is false for a malformed key.
func (k Key) Parts() (p KeyParts, ok bool) {
	if len(k) < 5 {
		return p, false
	}
	p.Type = binary.BigEndian.Uint16([]byte(k[0:2]))
	p.Class = binary.BigEndian.Uint16([]byte(k[2:4]))
	flags := k[4]
	p.DO, p.CD = flags&flagDO != 0, flags&flagCD != 0
	rest := k[5:]
	if flags&flagScoped != 0 {
		if len(rest) < 40 {
			return p, false
		}
		copy(p.Scope.ID[:], rest[:32])
		p.Scope.Epoch = binary.BigEndian.Uint64([]byte(rest[32:40]))
		rest = rest[40:]
	}
	if flags&(flagECS4|flagECS6) != 0 {
		size := 4
		if flags&flagECS6 != 0 {
			size = 16
		}
		if len(rest) < 1+size {
			return p, false
		}
		addr, _ := netip.AddrFromSlice([]byte(rest[1 : 1+size]))
		prefix, err := addr.Prefix(int(rest[0]))
		if err != nil {
			return p, false
		}
		p.ECS = prefix
		rest = rest[1+size:]
	}
	p.Name = string(rest)
	return p, true
}
