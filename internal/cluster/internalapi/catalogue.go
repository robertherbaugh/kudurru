package internalapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// PathCatalogue is GET /internal/catalogue: the blocked-services and
// safe-search catalogue document the node serves (D60). Replicas poll the
// leader's, which the leader downloads from blocking.catalogue.url, so
// every node serves the same catalogue without each fetching it. The body
// is the document byte for byte; X-Kudurru-Digest carries its sha256 and
// X-Kudurru-Source the URL it came from ("embedded" for the built-in one).
const PathCatalogue = "/internal/catalogue"

// maxCatalogueBytes caps a catalogue body (catalogue.MaxBytes).
const maxCatalogueBytes = 4 << 20

// ErrCatalogueUnsupported is returned by Client.Catalogue when the peer has
// no /internal/catalogue route (a 404 from an older node).
var ErrCatalogueUnsupported = errors.New("internalapi: peer does not serve " + PathCatalogue)

// CatalogueBody is the catalogue a node serves.
type CatalogueBody struct {
	Body []byte
	// Source is the URL the document came from, or "embedded".
	Source string
}

func (s *Server) handleCatalogue(w http.ResponseWriter, _ *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(statusWriteTimeout))
	c := s.opts.Catalogue()
	sum := sha256.Sum256(c.Body)
	digest := hex.EncodeToString(sum[:])
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(c.Body)))
	h.Set("ETag", `"`+digest+`"`)
	h.Set(HeaderDigest, digest)
	h.Set(HeaderSource, c.Source)
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(c.Body)
}

// Catalogue fetches GET /internal/catalogue from addr (host:port) and
// verifies the body against its digest header.
func (c *Client) Catalogue(ctx context.Context, addr string) (CatalogueBody, error) {
	resp, err := c.get(ctx, addr, PathCatalogue)
	if errors.Is(err, ErrListNotFound) {
		return CatalogueBody{}, ErrCatalogueUnsupported
	}
	if err != nil {
		return CatalogueBody{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogueBytes+1))
	if err != nil {
		return CatalogueBody{}, fmt.Errorf("internalapi: read catalogue from %s: %w", addr, err)
	}
	if len(body) > maxCatalogueBytes {
		return CatalogueBody{}, ErrTooLarge
	}
	sum := sha256.Sum256(body)
	if want := resp.Header.Get(HeaderDigest); want != hex.EncodeToString(sum[:]) {
		return CatalogueBody{}, ErrDigestMismatch
	}
	return CatalogueBody{Body: body, Source: resp.Header.Get(HeaderSource)}, nil
}
