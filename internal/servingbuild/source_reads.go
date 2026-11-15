package servingbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
)

const maxSourceReadBytes int64 = 64 << 20

// SourceBodyReads opens only bytes authorized by the original public object's
// digest receipt. The cache does not promise retention of an old source body.
type SourceBodyReads struct{ cache *listfetcher.DiskCache }

// NewSourceBodyReads requires the explicit local cache, without a publisher or
// fetcher. It cannot recapture a newer object or fetch a missing body.
func NewSourceBodyReads(cache *listfetcher.DiskCache) (*SourceBodyReads, error) {
	if cache == nil {
		return nil, serving.ErrComponents
	}
	return &SourceBodyReads{cache: cache}, nil
}

func sourceReadString(s string) bool { return len(s) <= 4096 && utf8.ValidString(s) }

// OpenSourceBody independently resolves the exact canonical source name from
// the original lease. A digest supplied by a caller is never authority. Fetched
// is deliberately absent: the captured object has no local fetch receipt.
func (r *SourceBodyReads) OpenSourceBody(ctx context.Context, namespace, listName, canonicalSourceName string) (io.ReadCloser, string, int64, error) {
	if r == nil || r.cache == nil || ctx == nil || namespace == "" || listName == "" || canonicalSourceName == "" || !sourceReadString(namespace) || !sourceReadString(listName) || !sourceReadString(canonicalSourceName) {
		return nil, "", 0, serving.ErrComponents
	}
	objects, _, err := NewObjectReads().ObjectSnapshot(ctx)
	if err != nil {
		return nil, "", 0, err
	}
	object, ok := objects.Get(store.Key{Kind: "BlockList", Namespace: namespace, Name: listName})
	list, typed := object.(*public.BlockList)
	if !ok || !typed || list == nil || list.Namespace != namespace || list.Name != listName || list.UID == "" || list.ResourceVersion == "" || !sourceReadString(string(list.UID)) || !sourceReadString(list.ResourceVersion) || len(list.Spec.Sources) > 256 || len(list.Status.Sources) > 256 {
		return nil, "", 0, serving.ErrComponents
	}
	seen := map[string]bool{}
	index := -1
	for i, src := range list.Spec.Sources {
		if !sourceReadString(src.Name) || !sourceReadString(src.URL) || src.ConfigMapRef != nil && (!sourceReadString(src.ConfigMapRef.Name) || !sourceReadString(src.ConfigMapRef.Namespace)) {
			return nil, "", 0, serving.ErrComponents
		}
		name := blocking.SourceName(i, src)
		if !sourceReadString(name) || seen[name] {
			return nil, "", 0, serving.ErrComponents
		}
		seen[name] = true
		if name == canonicalSourceName {
			index = i
		}
	}
	if index < 0 {
		return nil, "", 0, serving.ErrComponents
	}
	src := list.Spec.Sources[index]
	if blocking.IsRPZSource(src) || (src.URL == "") == (src.ConfigMapRef == nil) || len(src.Inline) != 0 || len(src.Entries) != 0 {
		return nil, "", 0, serving.ErrComponents
	}
	digest := ""
	seen = map[string]bool{}
	for _, status := range list.Status.Sources {
		if status.Name == "" || !sourceReadString(status.Name) || seen[status.Name] {
			return nil, "", 0, serving.ErrComponents
		}
		seen[status.Name] = true
		if status.Name == canonicalSourceName {
			digest = status.Digest
		}
	}
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return nil, "", 0, serving.ErrComponents
	}
	body, err := r.cache.OpenList(digest)
	if err != nil {
		return nil, "", 0, err
	}
	file, ok := body.Body.(*os.File)
	if !ok {
		_ = body.Body.Close()
		return nil, "", 0, serving.ErrComponents
	}
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || body.Size < 0 || body.Size > maxSourceReadBytes || info.Size() != body.Size || !info.Mode().IsRegular() {
		return nil, "", 0, serving.ErrComponents
	}
	hash := sha256.New()
	n, err := io.Copy(hash, sourceReadContext{ctx, io.LimitReader(file, maxSourceReadBytes+1)})
	if err != nil {
		return nil, "", 0, err
	}
	if n != body.Size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return nil, "", 0, listfetcher.ErrDigestMismatch
	}
	if _, _, err := NewObjectReads().ObjectSnapshot(ctx); err != nil {
		return nil, "", 0, err
	}
	// SectionReader starts at zero and prevents a later append from extending the
	// parsed body. Ordinary cache replacement uses rename and preserves this fd.
	success = true
	return &sourceReadBody{sourceReadContext{ctx, io.NewSectionReader(file, 0, body.Size)}, file}, digest, body.Size, nil
}

type sourceReadContext struct {
	ctx context.Context
	io.Reader
}

func (r sourceReadContext) Read(p []byte) (int, error) {
	if _, _, err := NewObjectReads().ObjectSnapshot(r.ctx); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type sourceReadBody struct {
	sourceReadContext
	io.Closer
}
