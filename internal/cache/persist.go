package cache

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// dumpMagic starts every dump; the byte after it is the format version.
const (
	dumpMagic   = "KDRC"
	dumpVersion = 1
	maxKeyLen   = 1024
	maxWireLen  = 65535
)

// ErrBadDump is returned by Load for input that is not a cache dump.
var ErrBadDump = errors.New("cache: not a kudurru cache dump")

// Dump writes every entry that is still within its stale window to w, least
// recently used first per shard so Load restores recency. It is the
// shutdown hook for local.db persistence; the bytes are opaque to callers.
// Times are absolute, so an entry loaded after a restart keeps counting
// down from when it was stored.
func (c *Cache) Dump(w io.Writer) error {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString(dumpMagic); err != nil {
		return err
	}
	if err := bw.WriteByte(dumpVersion); err != nil {
		return err
	}
	now := c.nanos()
	var buf [binary.MaxVarintLen64]byte
	putU := func(v uint64) { n := binary.PutUvarint(buf[:], v); _, _ = bw.Write(buf[:n]) }
	putI := func(v int64) { n := binary.PutVarint(buf[:], v); _, _ = bw.Write(buf[:n]) }
	for i := range c.shards {
		sh := &c.shards[i]
		sh.mu.Lock()
		snap := make([]entry, 0, len(sh.m))
		for e := sh.tail; e != nil; e = e.prev {
			if now < e.staleUntil {
				snap = append(snap, *e)
			}
		}
		sh.mu.Unlock()
		for _, e := range snap {
			putU(uint64(len(e.key)))
			_, _ = bw.WriteString(string(e.key))
			_ = bw.WriteByte(byte(e.kind))
			putI(e.stored)
			putI(e.expires)
			putI(e.staleUntil)
			putU(uint64(len(e.wire)))
			if _, err := bw.Write(e.wire); err != nil {
				return err
			}
		}
	}
	putU(0)
	return bw.Flush()
}

// Load adds the entries of a Dump, skipping those past their stale window,
// and returns how many it added. Capacity limits apply as for Set.
func (c *Cache) Load(r io.Reader) (int, error) {
	br := bufio.NewReader(r)
	head := make([]byte, len(dumpMagic)+1)
	if _, err := io.ReadFull(br, head); err != nil || string(head[:len(dumpMagic)]) != dumpMagic {
		return 0, ErrBadDump
	}
	if head[len(dumpMagic)] != dumpVersion {
		return 0, fmt.Errorf("%w: unsupported version %d", ErrBadDump, head[len(dumpMagic)])
	}
	now := c.nanos()
	n := 0
	for {
		klen, err := binary.ReadUvarint(br)
		if err != nil {
			return n, truncated(err)
		}
		if klen == 0 {
			return n, nil
		}
		if klen > maxKeyLen {
			return n, fmt.Errorf("%w: key length %d", ErrBadDump, klen)
		}
		key := make([]byte, klen)
		if _, err := io.ReadFull(br, key); err != nil {
			return n, truncated(err)
		}
		kind, err := br.ReadByte()
		if err != nil {
			return n, truncated(err)
		}
		if kind > byte(Negative) {
			return n, fmt.Errorf("%w: kind %d", ErrBadDump, kind)
		}
		var times [3]int64
		for i := range times {
			if times[i], err = binary.ReadVarint(br); err != nil {
				return n, truncated(err)
			}
		}
		wlen, err := binary.ReadUvarint(br)
		if err != nil {
			return n, truncated(err)
		}
		if wlen > maxWireLen {
			return n, fmt.Errorf("%w: message length %d", ErrBadDump, wlen)
		}
		wire := make([]byte, wlen)
		if _, err := io.ReadFull(br, wire); err != nil {
			return n, truncated(err)
		}
		stored, expires, staleUntil := times[0], times[1], times[2]
		if now >= staleUntil || expires < stored || staleUntil < expires {
			continue
		}
		c.insert(&entry{key: Key(key), wire: wire, kind: Kind(kind), stored: stored, expires: expires, staleUntil: staleUntil, windowStart: now})
		n++
	}
}

func truncated(err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("%w: %w", ErrBadDump, err)
}
