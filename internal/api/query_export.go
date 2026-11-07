package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const queryExportMaxBytes = 16 << 20

var errQueryExportSize = errors.New("query export exceeds the output limit")

type queryExportOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	CacheControl       string `header:"Cache-Control"`
	Body               []byte
}

func (s *Server) registerQueryExport() {
	op := operation(http.MethodGet, "/queries/export", "exportQueries", "Download one filtered query-log page as JSON (up to 1000 rows)", "querylog")
	op.Description = "Uses the same filters, scope, privacy and cursor as GET /queries. Defaults to 1000 rows. The JSON retains source, node errors, partial results and cursor_next; it is not an unbounded or cross-page snapshot export. Output is capped at 16 MiB and the read at 15 seconds. No attachment is sent on error or cancellation."
	register(s, op, perm{resource: "querylog", verb: "read"}, s.exportQueries)
}

func (s *Server) exportQueries(ctx context.Context, in *QueryParams) (*queryExportOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	params := *in
	if params.Limit == 0 {
		params.Limit = 1000
	}
	page, err := s.listQueries(ctx, &params)
	if err != nil {
		return nil, err
	}
	// Buffer one bounded page before committing HTTP headers. A failed peer is
	// still explicit in page metadata; cancellation never yields a truncated
	// attachment that could be mistaken for a complete export.
	buf := &queryExportBuffer{ctx: ctx}
	if err := json.NewEncoder(buf).Encode(page.Body); err != nil {
		if errors.Is(err, errQueryExportSize) {
			return nil, newError(http.StatusRequestEntityTooLarge, KeyInvalid, "query export exceeds 16 MiB", "reduce limit or narrow the query filters")
		}
		return nil, errUnavailable("query export interrupted")
	}
	if ctx.Err() != nil {
		return nil, errUnavailable("query export interrupted")
	}
	return &queryExportOutput{
		ContentType: "application/json", ContentDisposition: `attachment; filename="kudurru-queries.json"`,
		CacheControl: "no-store", Body: buf.Bytes(),
	}, nil
}

type queryExportBuffer struct {
	bytes.Buffer
	ctx context.Context
}

func (b *queryExportBuffer) Write(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > queryExportMaxBytes-b.Len() {
		return 0, errQueryExportSize
	}
	return b.Buffer.Write(p)
}
