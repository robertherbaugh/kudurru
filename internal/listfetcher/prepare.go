package listfetcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
)

// ErrIncomplete means a complete candidate cannot yet be compiled. The caller
// retains its previous WHOLE serving bundle, never individual old list results.
var ErrIncomplete = errors.New("listfetcher: complete candidate bodies unavailable")

// CompleteOptions bounds one candidate's list preparation. Catalogue must be
// the exact immutable catalogue used by that candidate's policy/blocking owners.
// Public objects must already have passed the local image/schema validator.
type CompleteOptions struct {
	Cache                  *DiskCache
	Catalogue              *catalogue.Catalogue
	Compile                blocking.CompileOptions
	MaxLists, MaxSources   int
	MaxBodyBytes, MaxBytes int64
}

// PreparedLists contains an immutable exact-input compilation. Preparation
// performs local disk reads only; the background fetcher supplies missing bodies.
type PreparedLists struct {
	Snapshot *blocking.Snapshot
	Identity string
}

// PrepareComplete compiles all enabled lists from one captured object cut.
// External text sources require their explicitly published digest and matching
// bytes at the configured source key. Empty/mismatched digests never authorize
// a cache fallback. File descriptors stay pinned across verification and parsing.
// RPZ remains an explicit external-policy list, with its own captured owner.
func PrepareComplete(ctx context.Context, objects []*public.BlockList, opts CompleteOptions) (*PreparedLists, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Cache == nil || opts.Catalogue == nil || opts.Compile.RegexHardCap < 0 {
		return nil, ErrIncomplete
	}
	if opts.MaxLists == 0 {
		opts.MaxLists = 4096
	}
	if opts.MaxSources == 0 {
		opts.MaxSources = 16384
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = DefaultMaxHTTPSize
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 256 << 20
	}
	if opts.MaxLists < 1 || opts.MaxLists > 10000 || opts.MaxSources < 1 || opts.MaxSources > 100000 || opts.MaxBodyBytes < 1 || opts.MaxBodyBytes > 256<<20 || opts.MaxBytes < 1 || opts.MaxBytes > 1<<30 || len(objects) > opts.MaxLists {
		return nil, ErrTooLarge
	}
	left := opts.MaxBytes
	count := 0
	seen := map[string]bool{}
	cloned := make([]*public.BlockList, 0, len(objects))
	for _, object := range objects {
		if object == nil || object.Name == "" {
			return nil, ErrIncomplete
		}
		name := ListName(object.Namespace, object.Name)
		if seen[name] {
			return nil, ErrIncomplete
		}
		seen[name] = true
		count += len(object.Spec.Sources)
		if count > opts.MaxSources {
			return nil, ErrTooLarge
		}
		raw, err := json.Marshal(object)
		if err != nil || int64(len(raw)) > left {
			return nil, ErrTooLarge
		}
		left -= int64(len(raw))
		cloned = append(cloned, object.DeepCopy())
	}
	slices.SortFunc(cloned, func(a, b *public.BlockList) int {
		return strings.Compare(ListName(a.Namespace, a.Name), ListName(b.Namespace, b.Name))
	})
	fingerprint := &fpBuilder{}
	fingerprint.add("complete-lists/v1", opts.Catalogue.Fingerprint())
	var compiled []*blocking.List
	for _, object := range cloned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if public.IsDisabled(object) || object.DeletionTimestamp != nil {
			continue
		}
		pl := plan(object, DefaultRefreshInterval, DefaultMinRefreshInterval)
		list, identity, err := compileComplete(ctx, pl, opts, &left)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, list)
		fingerprint.add(identity)
	}
	for _, spec := range opts.Catalogue.Specs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(compiled) >= opts.MaxLists {
			return nil, ErrTooLarge
		}
		list, err := blocking.Compile(contextSpec(ctx, spec), nil, opts.Compile)
		if err != nil {
			return nil, ErrIncomplete
		}
		compiled = append(compiled, list)
	}
	snapshot, err := blocking.NewSnapshot(1, compiled...)
	if err != nil {
		return nil, ErrIncomplete
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(fingerprint.String()))
	return &PreparedLists{Snapshot: snapshot, Identity: hex.EncodeToString(digest[:])}, nil
}

func compileComplete(ctx context.Context, pl plannedList, opts CompleteOptions, left *int64) (*blocking.List, string, error) {
	var opened openedSources
	defer opened.close()
	spec := blocking.Spec{Name: pl.name, Mode: pl.mode, ExternalPolicy: pl.externalPolicy()}
	fp := &fpBuilder{}
	fp.add(pl.name, string(pl.mode))
	// Duplicate source/status names are ambiguous even if a map would select
	// one. The canonical image must identify every digest unambiguously.
	seen := map[string]bool{}
	for _, status := range pl.obj.Status.Sources {
		if status.Name == "" || seen[status.Name] {
			return nil, "", ErrIncomplete
		}
		seen[status.Name] = true
	}
	seen = map[string]bool{}
	for i := range pl.sources {
		ps := &pl.sources[i]
		if seen[ps.name] {
			return nil, "", ErrIncomplete
		}
		seen[ps.name] = true
		switch {
		case ps.disabled:
			fp.add(ps.name, "disabled")
		case ps.external:
			fp.add(ps.name, "rpz")
		case ps.key == "":
			spec.Sources = append(spec.Sources, ps.compileSource())
			fp.add(ps.fp(inlineDigest(ps.inline))...)
		default:
			if ps.status == nil || !isHexName(ps.status.Digest) || strings.ToLower(ps.status.Digest) != ps.status.Digest {
				return nil, "", ErrIncomplete
			}
			file, entry, err := opts.Cache.Open(ps.key)
			if err != nil {
				return nil, "", ErrIncomplete
			}
			// Register the descriptor before any validation can return.
			source := opened.fileSource(ps.name, ps, file, entry.Size)
			if entry.Digest != ps.status.Digest || entry.Size < 0 || entry.Size > opts.MaxBodyBytes || entry.Size > *left {
				return nil, "", ErrIncomplete
			}
			info, err := file.Stat()
			if err != nil || info.Size() != entry.Size || !info.Mode().IsRegular() {
				return nil, "", ErrIncomplete
			}
			hash := sha256.New()
			n, err := io.Copy(hash, contextReader{ctx, io.NewSectionReader(file, 0, entry.Size)})
			if err != nil {
				return nil, "", err
			}
			if n != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.Digest {
				return nil, "", ErrDigestMismatch
			}
			*left -= entry.Size
			spec.Sources = append(spec.Sources, source)
			fp.add(ps.key)
			fp.add(ps.fp(entry.Digest)...)
		}
	}
	if len(spec.Sources) == 0 && !spec.ExternalPolicy {
		spec.Sources = []blocking.Source{emptySource()}
	}
	list, err := blocking.Compile(contextSpec(ctx, spec), nil, opts.Compile)
	if err != nil {
		return nil, "", ErrIncomplete
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return list, fp.String(), nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type contextReadCloser struct {
	contextReader
	io.Closer
}

func contextSpec(ctx context.Context, spec blocking.Spec) blocking.Spec {
	spec.Sources = slices.Clone(spec.Sources)
	for i := range spec.Sources {
		open := spec.Sources[i].Open
		spec.Sources[i].Open = func() (io.ReadCloser, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			reader, err := open()
			if err != nil {
				return nil, err
			}
			return contextReadCloser{contextReader{ctx, reader}, reader}, nil
		}
	}
	return spec
}
