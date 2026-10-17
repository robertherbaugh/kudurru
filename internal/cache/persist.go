package cache

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
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
	_, err := c.WriteDump(w)
	return err
}

// WriteDump is Dump that also returns how many entries it wrote.
func (c *Cache) WriteDump(w io.Writer) (int, error) {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString(dumpMagic); err != nil {
		return 0, err
	}
	if err := bw.WriteByte(dumpVersion); err != nil {
		return 0, err
	}
	written := 0
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
				return written, err
			}
			written++
		}
	}
	putU(0)
	return written, bw.Flush()
}

// PersistStats describes one Save or Restore of a Persister.
type PersistStats struct {
	// Entries is how many entries were written or loaded.
	Entries int
	// Bytes is the size of the stored dump.
	Bytes int
	// WrittenAt is when the dump was taken.
	WrittenAt time.Time
}

// Persister is the persistence hook contract of the cache: it keeps a dump
// of the live entries across restarts (dev-docs/design-record.md §3.3
// "persistence", the M1 carry-over "cache Dump/Load across restarts").
// internal/cache/persist implements it on a local.db blob.
//
// Restore runs once at start, before the DNS listeners serve, on an empty
// cache; a missing dump is not an error (zero stats), a dump that is too
// old or of another format returns an error the caller logs and ignores.
// Save may run at any time while the cache serves (Dump takes each shard
// lock only to copy its entries) and runs once more at shutdown.
type Persister interface {
	Save(ctx context.Context, c *Cache) (PersistStats, error)
	Restore(ctx context.Context, c *Cache) (PersistStats, error)
}

// RunPersister saves c through p every interval (never periodically when
// interval is zero) and once more when ctx ends, bounded by finalTimeout
// (5 s when zero). It is the always-on runnable serve adds next to Run;
// failures are logged and retried on the next tick.
func RunPersister(ctx context.Context, c *Cache, p Persister, interval, finalTimeout time.Duration, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if finalTimeout <= 0 {
		finalTimeout = 5 * time.Second
	}
	save := func(ctx context.Context, final bool) {
		st, err := p.Save(ctx, c)
		switch {
		case err != nil:
			logger.Warn("cache dump not saved", "err", err, "final", final)
		case final:
			logger.Info("cache dump saved", "entries", st.Entries, "bytes", st.Bytes)
		default:
			logger.Debug("cache dump saved", "entries", st.Entries, "bytes", st.Bytes)
		}
	}
	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalTimeout)
			save(fctx, true)
			cancel()
			return nil
		case <-tick:
			save(ctx, false)
		}
	}
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
