package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/serving"
)

// Convenience reads require the whole original publication, including dependency
// replacements at an unchanged canonical generation. This optional ObjectReads
// capability cannot recapture a successor or repair an incomplete wiring.
type convenienceReadIdentity interface {
	ObjectReadIdentity(context.Context) (serving.Identity, error)
}

type convenienceReadCursor struct {
	Version int              `json:"v"`
	Cut     serving.Identity `json:"cut"`
	Query   string           `json:"query"`
	Offset  int              `json:"offset"`
}

func (s *Server) capturedConvenienceReads() bool {
	return s.opts.ObjectReads != nil || s.opts.CanonicalSettings != nil || s.opts.ObjectCommand != nil ||
		s.opts.DomainCommand != nil || s.opts.UnblockCommand != nil || s.opts.SourceBodyReads != nil
}

func (s *Server) convenienceIdentity(ctx context.Context) (serving.Identity, error) {
	r, ok := s.opts.ObjectReads.(convenienceReadIdentity)
	if !ok || ctx == nil || ctx.Err() != nil {
		return serving.Identity{}, objectReadUnavailable()
	}
	id, err := r.ObjectReadIdentity(ctx)
	if err != nil || id.Base.Validate() != nil || id.Publication == 0 || id.Dependencies == "" || ctx.Err() != nil {
		return serving.Identity{}, objectReadUnavailable()
	}
	id.Base.Term.PrimaryName = ""
	return id, nil
}

func convenienceQuery(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func conveniencePageStart(raw string, cut serving.Identity, query string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	if len(raw) > maxObjectReadCursor {
		return 0, errInvalid("continue token is too large")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return 0, errInvalid("invalid continue token")
	}
	var c convenienceReadCursor
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Version != 1 || c.Cut.Base.Validate() != nil ||
		c.Cut.Publication == 0 || c.Cut.Dependencies == "" || c.Offset < 0 || c.Offset > 1000000 ||
		len(c.Query) != 64 || strings.Trim(c.Query, "0123456789abcdef") != "" {
		return 0, errInvalid("invalid continue token")
	}
	canonical, _ := json.Marshal(c)
	if !bytes.Equal(b, canonical) {
		return 0, errInvalid("noncanonical continue token")
	}
	if !c.Cut.Base.Same(cut.Base) || c.Cut.Publication != cut.Publication || c.Cut.Dependencies != cut.Dependencies || c.Query != query {
		return 0, newError(http.StatusConflict, KeyConflict, "pagination publication or query changed", "restart pagination without continue")
	}
	return c.Offset, nil
}

func conveniencePageNext(cut serving.Identity, query string, offset int) string {
	b, _ := json.Marshal(convenienceReadCursor{Version: 1, Cut: cut, Query: query, Offset: offset})
	return base64.RawURLEncoding.EncodeToString(b)
}
