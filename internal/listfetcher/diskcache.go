package listfetcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

const (
	metaSuffix = ".meta"
	tmpPrefix  = ".tmp-"
)

// Cache errors.
var (
	// ErrTooLarge: a body exceeded the size cap (maxHTTPSize).
	ErrTooLarge = errors.New("listfetcher: list body exceeds the size cap")
	// ErrDigestMismatch: a body pulled by digest hashed to something else.
	ErrDigestMismatch = errors.New("listfetcher: list body does not match the expected digest")
)

// Origins recorded in Entry.Origin.
const (
	OriginFetch  = "fetch"  // downloaded by the leader's fetcher
	OriginLeader = "leader" // pulled from the leader by digest
	OriginSelf   = "self"   // downloaded by a replica after the fallback delay
)

// Entry is the metadata of one cached body, stored next to it as
// <sha256(key)>.meta.
type Entry struct {
	// Key is the list URL, or configmap://<namespace>/<name>.
	Key string `json:"key"`
	// Digest is the lowercase hex sha256 of the body.
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	// Fetched is the last successful download or 304 revalidation.
	Fetched time.Time `json:"fetched"`
	// Changed is when the body last changed.
	Changed time.Time `json:"changed"`
	Origin  string    `json:"origin"`
	// Rules and Invalid are the parser's counts when the body was stored.
	Rules   int `json:"rules"`
	Invalid int `json:"invalid"`
}

// PutOptions control DiskCache.Put.
type PutOptions struct {
	// MaxSize caps the body; 0 means no cap.
	MaxSize int64
	// ExpectDigest rejects a body whose sha256 differs (pull by digest).
	ExpectDigest string
	// Validate inspects the complete temporary file before it replaces the
	// cached body; an error discards it (keep-old-on-failure). It returns
	// the parser's rule and invalid-line counts.
	Validate func(path string) (rules, invalid int, err error)
	// ETag, LastModified, Origin and Fetched are recorded in the Entry.
	ETag, LastModified, Origin string
	Fetched                    time.Time
}

// DiskCache is the list body cache under blocking.listsDir (default
// <dataDir>/lists): one file per source named sha256(key) plus a JSON
// sidecar, written tee-to-temp and renamed atomically. It is shared by the
// leader's fetcher, the replica resolver on the same node and the internal
// listener, which serves bodies by digest. Safe for concurrent use.
type DiskCache struct {
	dir string
	log *slog.Logger

	mu       sync.Mutex
	byKey    map[string]Entry
	byDigest map[string]string // digest -> key
	locks    map[string]*sync.Mutex
}

// FileName returns the cache file name of key: hex(sha256(key)).
func FileName(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// OpenDiskCache opens (creating) dir, drops temporary files left by a
// crash and verifies every cached body against its recorded digest,
// removing entries that do not match.
func OpenDiskCache(dir string, log *slog.Logger) (*DiskCache, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("listfetcher: lists dir: %w", err)
	}
	c := &DiskCache{dir: dir, log: log, byKey: map[string]Entry{}, byDigest: map[string]string{}, locks: map[string]*sync.Mutex{}}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("listfetcher: lists dir: %w", err)
	}
	known := map[string]bool{}
	for _, de := range des {
		name := de.Name()
		switch {
		case strings.HasPrefix(name, tmpPrefix):
			_ = os.Remove(filepath.Join(dir, name))
		case strings.HasSuffix(name, metaSuffix):
			e, err := c.loadEntry(strings.TrimSuffix(name, metaSuffix))
			if err != nil {
				c.log.Warn("listfetcher: dropping cache entry", "file", name, "err", err)
				c.removeFiles(strings.TrimSuffix(name, metaSuffix))
				continue
			}
			c.byKey[e.Key] = e
			c.byDigest[e.Digest] = e.Key
			known[FileName(e.Key)] = true
		}
	}
	for _, de := range des {
		name := de.Name()
		if !strings.HasPrefix(name, tmpPrefix) && !strings.HasSuffix(name, metaSuffix) && !known[name] && isHexName(name) {
			_ = os.Remove(filepath.Join(dir, name)) // body without metadata
		}
	}
	return c, nil
}

func isHexName(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (c *DiskCache) loadEntry(base string) (Entry, error) {
	b, err := os.ReadFile(filepath.Join(c.dir, base+metaSuffix)) //nolint:gosec // a file of the operator's list cache directory
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, err
	}
	if FileName(e.Key) != base {
		return Entry{}, fmt.Errorf("metadata names key %q", e.Key)
	}
	f, err := os.Open(filepath.Join(c.dir, base)) //nolint:gosec // a file of the operator's list cache directory
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Entry{}, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.Digest || n != e.Size {
		return Entry{}, fmt.Errorf("body hashes to %s (%d bytes), metadata says %s (%d bytes)", got, n, e.Digest, e.Size)
	}
	return e, nil
}

func (c *DiskCache) removeFiles(base string) {
	_ = os.Remove(filepath.Join(c.dir, base))
	_ = os.Remove(filepath.Join(c.dir, base+metaSuffix))
}

// Dir returns the cache directory.
func (c *DiskCache) Dir() string { return c.dir }

// Path returns the body file of key (which may not exist).
func (c *DiskCache) Path(key string) string { return filepath.Join(c.dir, FileName(key)) }

// Lookup returns the entry of key.
func (c *DiskCache) Lookup(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[key]
	return e, ok
}

// Keys returns every cached key, sorted.
func (c *DiskCache) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byKey))
	for k := range c.byKey {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (c *DiskCache) keyLock(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		c.locks[key] = l
	}
	return l
}

// Open opens the body of key together with its entry. The open file keeps
// the body stable even if a newer version is renamed over it meanwhile.
func (c *DiskCache) Open(key string) (*os.File, Entry, error) {
	l := c.keyLock(key)
	l.Lock()
	defer l.Unlock()
	e, ok := c.Lookup(key)
	if !ok {
		return nil, Entry{}, os.ErrNotExist
	}
	f, err := os.Open(c.Path(key))
	if err != nil {
		return nil, Entry{}, err
	}
	return f, e, nil
}

// Put streams r into a temporary file while hashing it, enforces the size
// cap and expected digest, runs Validate, and then atomically replaces the
// cached body of key. A body identical to the cached one only refreshes
// the metadata. On any error the previous body stays in place.
func (c *DiskCache) Put(key string, r io.Reader, opts PutOptions) (Entry, bool, error) {
	l := c.keyLock(key)
	l.Lock()
	defer l.Unlock()

	tmp, err := os.CreateTemp(c.dir, tmpPrefix+"*")
	if err != nil {
		return Entry{}, false, fmt.Errorf("listfetcher: cache temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	src := r
	if opts.MaxSize > 0 {
		src = io.LimitReader(r, opts.MaxSize+1)
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return Entry{}, false, err
	}
	if opts.MaxSize > 0 && n > opts.MaxSize {
		return Entry{}, false, fmt.Errorf("%w (%d bytes)", ErrTooLarge, opts.MaxSize)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if opts.ExpectDigest != "" && digest != opts.ExpectDigest {
		return Entry{}, false, fmt.Errorf("%w: got %s, want %s", ErrDigestMismatch, digest, opts.ExpectDigest)
	}
	if err := tmp.Sync(); err != nil {
		return Entry{}, false, err
	}
	if err := tmp.Close(); err != nil {
		return Entry{}, false, err
	}
	prev, hadPrev := c.Lookup(key)
	fetched := opts.Fetched
	if fetched.IsZero() {
		fetched = time.Now()
	}
	e := Entry{
		Key: key, Digest: digest, Size: n, ETag: opts.ETag, LastModified: opts.LastModified,
		Fetched: fetched.UTC(), Changed: fetched.UTC(), Origin: opts.Origin,
	}
	if hadPrev && prev.Digest == digest {
		e.Changed, e.Rules, e.Invalid = prev.Changed, prev.Rules, prev.Invalid
		if err := c.writeMeta(e); err != nil {
			return Entry{}, false, err
		}
		c.store(e)
		return e, false, nil
	}
	if opts.Validate != nil {
		rules, invalid, err := opts.Validate(tmpName)
		if err != nil {
			return Entry{}, false, err
		}
		e.Rules, e.Invalid = rules, invalid
	}
	if err := os.Rename(tmpName, c.Path(key)); err != nil {
		return Entry{}, false, fmt.Errorf("listfetcher: cache rename: %w", err)
	}
	committed = true
	if err := c.writeMeta(e); err != nil {
		return Entry{}, false, err
	}
	c.store(e)
	return e, true, nil
}

// Touch records a successful revalidation (HTTP 304) of key.
func (c *DiskCache) Touch(key string, fetched time.Time, etag, lastModified string) (Entry, error) {
	l := c.keyLock(key)
	l.Lock()
	defer l.Unlock()
	e, ok := c.Lookup(key)
	if !ok {
		return Entry{}, os.ErrNotExist
	}
	e.Fetched = fetched.UTC()
	if etag != "" {
		e.ETag = etag
	}
	if lastModified != "" {
		e.LastModified = lastModified
	}
	if err := c.writeMeta(e); err != nil {
		return Entry{}, err
	}
	c.store(e)
	return e, nil
}

// Remove drops key from the cache.
func (c *DiskCache) Remove(key string) {
	l := c.keyLock(key)
	l.Lock()
	defer l.Unlock()
	c.mu.Lock()
	if e, ok := c.byKey[key]; ok {
		delete(c.byKey, key)
		if c.byDigest[e.Digest] == key {
			delete(c.byDigest, e.Digest)
			for k, o := range c.byKey { // another key with the same body
				if o.Digest == e.Digest {
					c.byDigest[e.Digest] = k
					break
				}
			}
		}
	}
	c.mu.Unlock()
	c.removeFiles(FileName(key))
}

func (c *DiskCache) store(e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.byKey[e.Key]; ok && old.Digest != e.Digest && c.byDigest[old.Digest] == e.Key {
		delete(c.byDigest, old.Digest)
	}
	c.byKey[e.Key] = e
	c.byDigest[e.Digest] = e.Key
}

func (c *DiskCache) writeMeta(e Entry) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(c.dir, FileName(e.Key)+metaSuffix)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// OpenList implements internalapi.ListOpener: it serves the cached body
// whose sha256 is digest.
func (c *DiskCache) OpenList(digest string) (internalapi.ListBody, error) {
	c.mu.Lock()
	key, ok := c.byDigest[digest]
	c.mu.Unlock()
	if !ok {
		return internalapi.ListBody{}, internalapi.ErrListNotFound
	}
	f, e, err := c.Open(key)
	if errors.Is(err, os.ErrNotExist) || (err == nil && e.Digest != digest) {
		if f != nil {
			_ = f.Close()
		}
		return internalapi.ListBody{}, internalapi.ErrListNotFound
	}
	if err != nil {
		return internalapi.ListBody{}, err
	}
	return internalapi.ListBody{Body: f, Size: e.Size, Source: e.Key}, nil
}
