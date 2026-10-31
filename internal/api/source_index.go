package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

const (
	sourceIndexChunk     = 1024
	sourceIndexDataChunk = 64 << 10
	// Fixed-size chunks avoid unaccounted slice-growth copies. A builder stops
	// indexing at this bound and the same parse continues serving the page.
	maxSourceIndexBytes   = 96 << 20
	maxSourceIndexesBytes = 128 << 20
	maxSourceIndexes      = 16
)

type sourceIndexKey struct {
	digest     string
	format     parser.Format
	subdomains bool
}

type sourceIndexedRule struct {
	block, offset, length, line uint32
}

// sourceIndex contains normalized parser results only. It grants no authority
// to read a body, and contains no object, source URL, credential or exclusions.
type sourceIndex struct {
	chunks []*[sourceIndexChunk]sourceIndexedRule
	data   [][]byte
	count  int
	bytes  int
}

func (x *sourceIndex) add(value string, line, budget int) bool {
	// Compact references and fixed-size data blocks avoid per-rule allocation
	// overhead or retaining the parser's reusable buffers. Account for allocated
	// capacity and conservatively for chunk-slice backing arrays.
	allocation := 0
	dataCapacity := (max(sourceIndexDataChunk, len(value)) + 8191) &^ 8191
	newRules := x.count%sourceIndexChunk == 0
	newData := len(x.data) == 0 || cap(x.data[len(x.data)-1])-len(x.data[len(x.data)-1]) < len(value)
	if newRules {
		allocation += 16*sourceIndexChunk + 32
	}
	if newData {
		allocation += dataCapacity + 64
	}
	if allocation > budget-x.bytes {
		return false
	}
	if newRules {
		x.chunks = append(x.chunks, new([sourceIndexChunk]sourceIndexedRule))
	}
	if newData {
		x.data = append(x.data, make([]byte, 0, dataCapacity))
	}
	block := len(x.data) - 1
	offset := len(x.data[block])
	x.data[block] = append(x.data[block], value...)
	// Every component is below the 96 MiB input/index bound.
	x.chunks[x.count/sourceIndexChunk][x.count%sourceIndexChunk] = sourceIndexedRule{uint32(block), uint32(offset), uint32(len(value)), uint32(line)} //nolint:gosec // bounded by source/index limits
	x.count++
	x.bytes += allocation
	return true
}

func (x *sourceIndex) at(i int) (string, int) {
	rule := x.chunks[i/sourceIndexChunk][i%sourceIndexChunk]
	return string(x.data[rule.block][rule.offset : rule.offset+rule.length]), int(rule.line)
}

// page applies the original request's filters and exclusions. Plain pagination
// is O(page size); filtered/excluded pages scan normalized rules, never reparse.
func (x *sourceIndex) page(ctx context.Context, pg *entryPager, ex excludeSet) error {
	if pg.q == "" && len(ex) == 0 {
		pg.out.Total = x.count
		if pg.enabled == "false" {
			return ctx.Err()
		}
		pg.out.Matched = x.count
		end := min(pg.start+pg.limit, x.count)
		for i := pg.start; i < end; i++ {
			value, line := x.at(i)
			pg.out.Items = append(pg.out.Items, SourceEntry{Value: value, Line: line, Enabled: true})
		}
		if end < x.count {
			// Complete serving replaces this with its generation-bound cursor.
			pg.out.Continue = encodeCursor(strconv.Itoa(end))
		}
		return ctx.Err()
	}
	for i := range x.count {
		if i%sourceIndexChunk == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		value, line := x.at(i)
		pg.add(SourceEntry{Value: value, Line: line, Enabled: !ex.hit(value)})
	}
	return ctx.Err()
}

type sourceIndexEntry struct {
	index *sourceIndex
	used  uint64
}

// sourceIndexCache is local to the API server. Eviction drops only derived
// data; four admitted body reads bound indexes pinned by readers/builders.
type sourceIndexCache struct {
	mu      sync.Mutex
	entries map[sourceIndexKey]sourceIndexEntry
	bytes   int
	clock   uint64
}

func (c *sourceIndexCache) get(key sourceIndexKey) *sourceIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil
	}
	c.clock++
	entry.used = c.clock
	c.entries[key] = entry
	return entry.index
}

func (c *sourceIndexCache) put(key sourceIndexKey, index *sourceIndex) {
	if index == nil || index.bytes > maxSourceIndexBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[sourceIndexKey]sourceIndexEntry)
	}
	if _, exists := c.entries[key]; exists {
		return // a concurrent parse already published the same derived data
	}
	for len(c.entries) >= maxSourceIndexes || c.bytes+index.bytes > maxSourceIndexesBytes {
		var oldest sourceIndexKey
		used := ^uint64(0)
		for key, entry := range c.entries {
			if entry.used < used {
				oldest, used = key, entry.used
			}
		}
		c.bytes -= c.entries[oldest].index.bytes
		delete(c.entries, oldest)
	}
	c.clock++
	c.entries[key] = sourceIndexEntry{index, c.clock}
	c.bytes += index.bytes
}

// scanIndexedSource is called only after an admitted body acquisition. The
// input is bounded by its verified length; failed parses never publish an index.
func (s *Server) scanIndexedSource(ctx context.Context, key sourceIndexKey, body io.Reader, pg *entryPager, ex excludeSet, budget int) error {
	if index := s.sourceIndexes.get(key); index != nil {
		return index.page(ctx, pg, ex)
	}
	index := &sourceIndex{}
	_, err := parser.Parse(sourceAPIReader{ctx, body}, parser.Options{Format: key.format, HostsSubdomains: key.subdomains, KeepInvalid: -1, KeepUnsupported: -1}, func(rule parser.Rule) error {
		value := rule.String()
		pg.add(SourceEntry{Value: value, Line: rule.Line, Enabled: !ex.hit(value)})
		if index != nil && !index.add(value, rule.Line, budget) {
			index = nil // oversized expansion continues through bounded streaming
		}
		return ctx.Err()
	})
	if err == nil && ctx.Err() == nil {
		s.sourceIndexes.put(key, index)
	}
	if err == nil {
		return ctx.Err()
	}
	return err
}

// verifiedLegacyIndexBody retains the legacy caller's source selection. Only
// regular disk files with a verified digest enter the derived index. Other
// ListBodies implementations keep their existing streaming semantics.
func verifiedLegacyIndexBody(ctx context.Context, body io.ReadCloser, info SourceBody) (io.Reader, bool, error) {
	file, ok := body.(*os.File)
	if !ok || len(info.Digest) != 64 || strings.Trim(info.Digest, "0123456789abcdef") != "" || info.Size < 0 || info.Size > maxCapturedSourceBytes {
		return nil, false, nil
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != info.Size {
		return nil, false, sourceReadUnavailable()
	}
	hash := sha256.New()
	n, err := io.Copy(hash, sourceAPIReader{ctx, io.NewSectionReader(file, 0, info.Size+1)})
	if err != nil || n != info.Size || hex.EncodeToString(hash.Sum(nil)) != info.Digest {
		return nil, false, sourceReadUnavailable()
	}
	return io.NewSectionReader(file, 0, info.Size), true, nil
}
