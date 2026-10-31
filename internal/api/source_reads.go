package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// SourceBodyReadSource opens verified bytes authorized by the original
// BlockList's digest receipt. The descriptor belongs to the synchronous handler.
type SourceBodyReadSource interface {
	OpenSourceBody(context.Context, string, string, string) (body io.ReadCloser, digest string, size int64, err error)
}

const maxCapturedSourceBytes = 64 << 20

type sourceReadCursor struct {
	Version int                    `json:"v"`
	Base    replication.Generation `json:"base"`
	Query   string                 `json:"query"`
	Offset  int                    `json:"offset"`
}

func sourceReadUnavailable() error {
	return errUnavailable("original source body or receipt unavailable")
}

func boundedSourceString(s string, limit int) bool { return len(s) <= limit && utf8.ValidString(s) }

// Resolve and authorize the original object before inspecting cursor or cache.
// Shared getBlockList/findSource and mutation paths keep their legacy semantics.
func (s *Server) capturedSource(ctx context.Context, in *sourceEntriesInput) (*public.BlockList, int, replication.Generation, error) {
	objects, base, err := s.capturedObjects(ctx)
	if err != nil {
		return nil, 0, base, err
	}
	k, _ := kindByPlural("blocklists")
	object, found := objects.Get(store.Key{Kind: k.kind, Namespace: in.Namespace, Name: in.Name})
	if !found {
		if err := s.require(ctx, k.resource, "read", s.guessScope(k, in.Name), in.Name); err != nil {
			return nil, 0, base, err
		}
		return nil, 0, base, errNotFound(in.Namespace + "/" + in.Name)
	}
	if err := s.require(ctx, k.resource, "read", objectScope(k, object), in.Name); err != nil {
		return nil, 0, base, err
	}
	bl, ok := object.(*public.BlockList)
	if !ok || bl == nil || bl.Namespace != in.Namespace || bl.Name != in.Name || bl.UID == "" || bl.ResourceVersion == "" || !boundedSourceString(string(bl.UID), 4096) || !boundedSourceString(bl.ResourceVersion, 4096) || len(bl.Spec.Sources) > 256 || len(bl.Status.Sources) > 256 {
		return nil, 0, base, sourceReadUnavailable()
	}
	seen := map[string]bool{}
	idx := -1
	for i, src := range bl.Spec.Sources {
		if !boundedSourceString(src.Name, 4096) || !boundedSourceString(src.URL, 4096) || src.ConfigMapRef != nil && (!boundedSourceString(src.ConfigMapRef.Name, 4096) || !boundedSourceString(src.ConfigMapRef.Namespace, 4096)) {
			return nil, 0, base, sourceReadUnavailable()
		}
		name := blocking.SourceName(i, src)
		if seen[name] || !boundedSourceString(name, 4096) {
			return nil, 0, base, sourceReadUnavailable()
		}
		seen[name] = true
		if name == in.Source {
			idx = i
		}
	}
	if n, err := strconv.Atoi(in.Source); err == nil && n >= 0 && n < len(bl.Spec.Sources) {
		if idx >= 0 && idx != n {
			return nil, 0, base, sourceReadUnavailable()
		}
		idx = n
	}
	if idx < 0 {
		return nil, 0, base, errNotFound("source " + in.Source)
	}
	seen = map[string]bool{}
	for _, status := range bl.Status.Sources {
		if status.Name == "" || !boundedSourceString(status.Name, 4096) || seen[status.Name] {
			return nil, 0, base, sourceReadUnavailable()
		}
		seen[status.Name] = true
	}
	src := &bl.Spec.Sources[idx]
	if blocking.IsRPZSource(*src) {
		return nil, 0, base, newError(http.StatusUnprocessableEntity, KeyUnprocessable, "RPZ source entries are not line-source bodies", "")
	}
	// Bound selected parser/exclusion inputs before any copy, join, or JSON hash.
	if len(src.Inline) > 1000000 || len(src.Entries) > 10000 || len(src.Exclude) > 10000 || !boundedSourceString(string(src.Format), 64) {
		return nil, 0, base, sourceReadUnavailable()
	}
	left := maxCapturedSourceBytes
	check := func(value string) bool {
		left -= len(value)
		return left >= 0 && boundedSourceString(value, 1<<20)
	}
	for _, values := range [][]string{src.Inline, src.Exclude} {
		for _, value := range values {
			if !check(value) {
				return nil, 0, base, sourceReadUnavailable()
			}
		}
	}
	for _, entry := range src.Entries {
		if !check(entry.Value) || !check(entry.Comment) || !check(string(entry.Type)) || !check(entry.CreatedBy) || !check(string(entry.Origin)) || !check(entry.Request) {
			return nil, 0, base, sourceReadUnavailable()
		}
	}
	// Multiple competing body forms cannot be labeled as one source.
	forms := 0
	for _, present := range []bool{src.URL != "", src.ConfigMapRef != nil, len(src.Entries) != 0, len(src.Inline) != 0} {
		if present {
			forms++
		}
	}
	if forms > 1 {
		return nil, 0, base, sourceReadUnavailable()
	}
	return bl, idx, base, nil
}

func decodeSourceReadCursor(raw string) (sourceReadCursor, error) {
	var cursor sourceReadCursor
	if len(raw) > maxObjectReadCursor {
		return cursor, errInvalid("source continue token is too large")
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || !utf8.Valid(data) {
		return cursor, errInvalid("invalid source continue token")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&cursor) != nil || d.Decode(new(any)) != io.EOF || cursor.Version != 1 || cursor.Base.Validate() != nil || len(cursor.Query) != 64 || strings.Trim(cursor.Query, "0123456789abcdef") != "" || cursor.Offset < 0 || cursor.Offset > maxCapturedSourceBytes {
		return sourceReadCursor{}, errInvalid("invalid source continue token")
	}
	canonical, _ := json.Marshal(cursor)
	if !bytes.Equal(data, canonical) {
		return sourceReadCursor{}, errInvalid("noncanonical source continue token")
	}
	return cursor, nil
}

func (s *Server) listCapturedSourceEntries(ctx context.Context, in *sourceEntriesInput) (*sourceEntriesOutput, error) {
	if s.opts.ObjectReads == nil || s.opts.SourceBodyReads == nil || ctx == nil || ctx.Err() != nil {
		return nil, sourceReadUnavailable()
	}
	for _, value := range []string{in.Namespace, in.Name, in.Source, in.Q} {
		if !boundedSourceString(value, 4096) {
			return nil, errInvalid("source query is too large or invalid UTF-8")
		}
	}
	if in.Enabled != "" && in.Enabled != "true" && in.Enabled != "false" {
		return nil, errInvalid("enabled must be true or false")
	}
	bl, idx, base, err := s.capturedSource(ctx, in)
	if err != nil {
		return nil, err
	}
	src := &bl.Spec.Sources[idx]
	name := blocking.SourceName(idx, *src)
	kind := sourceKind(src)
	digest := ""
	if kind == "url" || kind == "configMap" {
		for _, status := range bl.Status.Sources {
			if status.Name == name {
				digest = status.Digest
			}
		}
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
			return nil, sourceReadUnavailable()
		}
	}
	release, err := s.acquireBodyScan(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	q, limit := strings.ToLower(strings.TrimSpace(in.Q)), clampLimit(in.Limit, 100, 1000)
	data, err := json.Marshal(struct {
		Namespace, Name, UID, RV, Source, Digest, Q, Enabled string
		Index, Limit                                         int
		Spec                                                 *public.ListSource
	}{bl.Namespace, bl.Name, string(bl.UID), bl.ResourceVersion, name, digest, q, in.Enabled, idx, limit, src})
	if err != nil || len(data) > 2*maxCapturedSourceBytes {
		return nil, sourceReadUnavailable()
	}
	hash := sha256.Sum256(data)
	query := hex.EncodeToString(hash[:])
	start := 0
	if in.Continue != "" {
		cursor, err := decodeSourceReadCursor(in.Continue)
		if err != nil {
			return nil, err
		}
		if !cursor.Base.Same(base) || cursor.Query != query {
			return nil, newError(http.StatusConflict, KeyConflict, "source pagination generation or query changed", "restart pagination without continue")
		}
		start = cursor.Offset
	}
	out := &SourceEntries{Items: []SourceEntry{}, StaleExcludes: []string{}, Kind: kind, Digest: digest}
	pg := &entryPager{q: q, enabled: in.Enabled, start: start, limit: limit, out: out}
	ex := newExcludeSet(src.Exclude)
	switch kind {
	case "entries":
		for i, entry := range src.Entries {
			if ctx.Err() != nil {
				return nil, sourceReadUnavailable()
			}
			pg.add(SourceEntry{Value: public.EntryRule(entry.Type, entry.Value), Line: i + 1, Enabled: entry.IsEnabled()})
		}
	case "inline":
		rules, off := make([]int, len(src.Inline)), make([]int, len(src.Inline))
		_, err = parser.Parse(sourceAPIReader{ctx, strings.NewReader(strings.Join(src.Inline, "\n"))}, parser.Options{Format: parser.Format(src.Format), HostsSubdomains: src.Subdomains, KeepInvalid: -1, KeepUnsupported: -1}, func(rule parser.Rule) error {
			if i := rule.Line - 1; i >= 0 && i < len(rules) {
				rules[i]++
				if ex.hit(rule.String()) {
					off[i]++
				}
			}
			return ctx.Err()
		})
		for i, line := range src.Inline {
			pg.add(SourceEntry{Value: line, Line: i + 1, Enabled: rules[i] == 0 || off[i] < rules[i]})
		}
		out.StaleExcludes = ex.stale()
	default:
		body, actualDigest, size, openErr := s.opts.SourceBodyReads.OpenSourceBody(ctx, bl.Namespace, bl.Name, name)
		if body != nil {
			defer func() { _ = body.Close() }()
		}
		if openErr != nil || body == nil || actualDigest != digest || size < 0 || size > maxCapturedSourceBytes {
			return nil, sourceReadUnavailable()
		}
		// Opening and verifying the exact captured body remains mandatory even
		// on a hit. A derived index must never revive missing bytes or authority.
		key := sourceIndexKey{digest, parser.Format(src.Format), src.Subdomains}
		err = s.scanIndexedSource(ctx, key, io.LimitReader(body, size+1), pg, ex, maxSourceIndexBytes)
		out.StaleExcludes = ex.stale()
	}
	if ctx.Err() != nil || err != nil && !errors.Is(err, io.EOF) {
		return nil, sourceReadUnavailable()
	}
	if out.Continue != "" {
		base.Term.PrimaryName = ""
		data, err := json.Marshal(sourceReadCursor{1, base, query, start + len(out.Items)})
		if err != nil || base64.RawURLEncoding.EncodedLen(len(data)) > maxObjectReadCursor {
			return nil, sourceReadUnavailable()
		}
		out.Continue = base64.RawURLEncoding.EncodeToString(data)
	}
	if _, finalBase, err := s.capturedObjects(ctx); err != nil || !finalBase.Same(base) {
		return nil, sourceReadUnavailable()
	}
	return &sourceEntriesOutput{ETag: bl.ResourceVersion, Body: out}, nil
}

type sourceAPIReader struct {
	ctx context.Context
	io.Reader
}

func (r sourceAPIReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
