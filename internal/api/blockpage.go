package api

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/robertherbaugh/kudurru/internal/blockpage"
)

// BlockPagePreviewer renders the block page for the admin preview;
// *blockpage.Server implements it.
type BlockPagePreviewer interface {
	Preview(ctx context.Context, domain string, client netip.Addr) []byte
}

type blockPagePreviewInput struct {
	Domain string `query:"domain" maxLength:"253" doc:"Blocked name to show; default blocked.example. The reason shown is an example: the preview reads neither the query log nor the policies."`
}

type blockPagePreviewOutput struct {
	ContentType  string `header:"Content-Type"`
	CSP          string `header:"Content-Security-Policy"`
	FrameOptions string `header:"X-Frame-Options"`
	CacheControl string `header:"Cache-Control"`
	Body         []byte
}

// registerBlockPage declares GET /blockpage/preview: the block page HTML
// with the current blockPage.* settings, for the Settings page's Preview
// button. Its CSP is the page's own, with framing allowed by the
// dashboard's origin.
func (s *Server) registerBlockPage() {
	op := operation(http.MethodGet, "/blockpage/preview", "previewBlockPage", "Render the block page (text/html) with the current blockPage settings", "settings")
	op.Description = "The same HTML the block page listener serves, marked as a preview. It renders whether or not blockPage.enabled is set."
	register(s, op, perm{resource: "settings", verb: "read"}, func(ctx context.Context, in *blockPagePreviewInput) (*blockPagePreviewOutput, error) {
		if s.opts.BlockPage == nil {
			return nil, errUnavailable("the block page is not available on this node")
		}
		return &blockPagePreviewOutput{
			ContentType:  "text/html; charset=utf-8",
			CSP:          blockpage.PreviewCSP,
			FrameOptions: "SAMEORIGIN",
			CacheControl: "no-store",
			Body:         s.opts.BlockPage.Preview(ctx, in.Domain, ClientIP(ctx)),
		}, nil
	})
}
