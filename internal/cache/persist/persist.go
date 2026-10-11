// Package persist keeps the response cache across restarts in local.db
// (the M1 carry-over "cache Dump/Load across restarts"): it implements
// cache.Persister on a localdb.BlobStore blob.
//
// The blob is a small header, then the cache.Dump bytes (gzip-compressed
// by default):
//
//	"KDRP" | version (1 byte) | flags (1 byte; bit 0 = gzip) |
//	written-at, Unix nanoseconds (8 bytes, big endian) | payload
//
// Restore ignores a dump that is older than MaxAge or carries another
// version; entries inside a dump keep their absolute expiry times, so
// cache.Load also drops every entry that expired (past its stale window)
// while the process was down.
//
// Serve wiring: open local.db for every driver, Restore before the DNS
// listeners serve, and run cache.RunPersister as an always-on runnable so
// the dump is refreshed periodically and written once more at shutdown.
package persist

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// Defaults.
const (
	// BlobName is the local.db blob holding the dump.
	BlobName = "cache.dump"
	// DefaultMaxAge is the oldest dump Restore accepts.
	DefaultMaxAge = 24 * time.Hour
)

const (
	magic      = "KDRP"
	version    = 1
	headerLen  = len(magic) + 2 + 8
	flagGzip   = 1 << 0
	knownFlags = flagGzip
)

// Sentinel errors returned by Restore; the cache stays empty and serving
// continues.
var (
	// ErrStale means the dump is older than MaxAge.
	ErrStale = errors.New("persist: cache dump is too old")
	// ErrIncompatible means the blob is not a dump this binary reads.
	ErrIncompatible = errors.New("persist: incompatible cache dump")
)

// Options configure a Persister.
type Options struct {
	// Name is the blob name; default BlobName.
	Name string
	// MaxAge is the oldest dump Restore accepts; default DefaultMaxAge.
	MaxAge time.Duration
	// NoCompress stores the dump uncompressed (larger blob, cheaper save).
	NoCompress bool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Persister implements cache.Persister on a BlobStore.
type Persister struct {
	blobs localdb.BlobStore
	opts  Options
}

var _ cache.Persister = (*Persister)(nil)

// New returns a Persister storing into blobs.
func New(blobs localdb.BlobStore, opts Options) (*Persister, error) {
	if blobs == nil {
		return nil, errors.New("persist: nil BlobStore")
	}
	if opts.Name == "" {
		opts.Name = BlobName
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Persister{blobs: blobs, opts: opts}, nil
}

// Save dumps c and replaces the stored blob.
func (p *Persister) Save(ctx context.Context, c *cache.Cache) (cache.PersistStats, error) {
	now := p.opts.Now()
	var buf bytes.Buffer
	flags := byte(0)
	if !p.opts.NoCompress {
		flags |= flagGzip
	}
	buf.WriteString(magic)
	buf.WriteByte(version)
	buf.WriteByte(flags)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(now.UnixNano())) // #nosec G115 -- post-1970 clock
	buf.Write(ts[:])

	var n int
	var err error
	if flags&flagGzip != 0 {
		zw, zerr := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		if zerr != nil {
			return cache.PersistStats{}, zerr
		}
		if n, err = c.WriteDump(zw); err == nil {
			err = zw.Close()
		}
	} else {
		n, err = c.WriteDump(&buf)
	}
	if err != nil {
		return cache.PersistStats{}, fmt.Errorf("persist: dump: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return cache.PersistStats{}, err
	}
	if err := p.blobs.PutBlob(ctx, p.opts.Name, buf.Bytes()); err != nil {
		return cache.PersistStats{}, err
	}
	return cache.PersistStats{Entries: n, Bytes: buf.Len(), WrittenAt: now}, nil
}

// Restore loads the stored dump into c. No stored dump yields zero stats
// and no error; a dump older than MaxAge returns ErrStale and one this
// binary cannot read ErrIncompatible (or cache.ErrBadDump when the payload
// is damaged; entries before the damage stay loaded).
func (p *Persister) Restore(ctx context.Context, c *cache.Cache) (cache.PersistStats, error) {
	b, err := p.blobs.GetBlob(ctx, p.opts.Name)
	if errors.Is(err, localdb.ErrNoBlob) {
		return cache.PersistStats{}, nil
	}
	if err != nil {
		return cache.PersistStats{}, err
	}
	st := cache.PersistStats{Bytes: len(b.Data)}
	data := b.Data
	if len(data) < headerLen || string(data[:len(magic)]) != magic {
		return st, fmt.Errorf("%w: bad header", ErrIncompatible)
	}
	if v := data[len(magic)]; v != version {
		return st, fmt.Errorf("%w: version %d, this binary reads %d", ErrIncompatible, v, version)
	}
	flags := data[len(magic)+1]
	if flags&^knownFlags != 0 {
		return st, fmt.Errorf("%w: unknown flags %#x", ErrIncompatible, flags)
	}
	st.WrittenAt = time.Unix(0, int64(binary.BigEndian.Uint64(data[len(magic)+2:headerLen]))) // #nosec G115 -- written from UnixNano
	if age := p.opts.Now().Sub(st.WrittenAt); age > p.opts.MaxAge {
		return st, fmt.Errorf("%w: written %s ago, maximum %s", ErrStale, age.Round(time.Second), p.opts.MaxAge)
	}
	var r io.Reader = bytes.NewReader(data[headerLen:])
	if flags&flagGzip != 0 {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return st, fmt.Errorf("%w: %w", cache.ErrBadDump, err)
		}
		defer func() { _ = zr.Close() }()
		r = bufio.NewReader(zr)
	}
	st.Entries, err = c.Load(r)
	return st, err
}
