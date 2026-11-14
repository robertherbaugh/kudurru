// Package clickhouse is the ClickHouse sink of querylog.sinks (D371): each
// batch is one INSERT ... FORMAT JSONEachRow over the HTTP interface, sent
// with the standard library client (internal/querylog/sinks/httpbatch).
// The rows are the records of internal/querylog/sinks/record; the table
// they fit is documented in docs/deploy/query-log-sinks.md.
package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/httpbatch"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/record"
)

// ExceptionCodeHeader carries ClickHouse's numeric error code on a failed
// request. It is the one detail of a refusal the sink reports: the response
// body can quote the rows it could not parse.
const ExceptionCodeHeader = "X-ClickHouse-Exception-Code"

// Options are the node's own inputs to every ClickHouse sink.
type Options struct {
	// Resolve reads credential references.
	Resolve httpbatch.Resolver
	Logger  *slog.Logger
	// UserAgent is sent with every request.
	UserAgent string
}

// Sink inserts batches into one ClickHouse table.
type Sink struct {
	client *httpbatch.Client
	body   bytes.Buffer
}

// New builds the sink of one querylog.sinks entry from its clickhouse
// object. It validates the settings and does no I/O.
func New(spec querylog.SinkSpec, o Options) (*Sink, error) {
	var settings config.QuerylogClickHouse
	if len(spec.Config) == 0 {
		return nil, errors.New("the clickhouse settings are missing")
	}
	if err := json.Unmarshal(spec.Config, &settings); err != nil {
		return nil, errors.New("the clickhouse settings are malformed")
	}
	if settings.URL == "" {
		return nil, errors.New("clickhouse.url is required")
	}
	if errs := settings.Validate(config.QuerylogSinkClickHouse); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if !config.ValidCompression(settings.Compression) {
		return nil, errors.New("clickhouse.compression is not gzip or none")
	}
	target, err := InsertURL(settings)
	if err != nil {
		return nil, err
	}
	header := http.Header{"Content-Type": {"application/x-ndjson"}}
	if o.UserAgent != "" {
		header.Set("User-Agent", o.UserAgent)
	}
	client, err := httpbatch.New(httpbatch.Options{
		Destination: "ClickHouse", URL: target, Header: header,
		Username: settings.Username, Password: settings.PasswordRef, CA: settings.CARef,
		Gzip: config.GzipBody(settings.Compression), Resolve: o.Resolve, Logger: o.Logger, Detail: exceptionCode,
	})
	if err != nil {
		return nil, err
	}
	return &Sink{client: client}, nil
}

// InsertURL is the request URL of one batch. The statement names the table
// and the format; the rows are the request body. Two input settings make the
// rows independent of the table's details: best_effort reads the RFC 3339
// timestamps into any DateTime column, and unknown fields are skipped, so a
// table created for an earlier kudurru keeps accepting rows when a later
// version adds a field. wait_end_of_query makes ClickHouse hold its answer
// until the insert has finished, so that the status is the outcome: a 2xx
// is never followed by an error in the body.
func InsertURL(settings config.QuerylogClickHouse) (*url.URL, error) {
	base, err := config.ParseSinkURL(settings.URL)
	if err != nil {
		return nil, fmt.Errorf("clickhouse.url %w", err)
	}
	target := *base
	if target.Path == "" {
		target.Path = "/"
	}
	query := url.Values{}
	// The names are restricted to letters, digits and underscores
	// (config.QuerylogClickHouse.Validate), so the quoting cannot be left.
	query.Set("query", "INSERT INTO `"+settings.DatabaseName()+"`.`"+settings.TableName()+"` FORMAT JSONEachRow")
	query.Set("date_time_input_format", "best_effort")
	query.Set("input_format_skip_unknown_fields", "1")
	query.Set("wait_end_of_query", "1")
	target.RawQuery = query.Encode()
	return &target, nil
}

// exceptionCode reports ClickHouse's numeric error code, e.g. 60 for an
// unknown table.
func exceptionCode(h http.Header) string {
	code := strings.TrimSpace(h.Get(ExceptionCodeHeader))
	if code == "" || len(code) > 6 || strings.Trim(code, "0123456789") != "" {
		return ""
	}
	return "ClickHouse exception code " + code
}

// Name implements querylog.Sink.
func (s *Sink) Name() string { return config.QuerylogSinkClickHouse }

// Open reads the credential references. It does not contact ClickHouse.
func (s *Sink) Open(ctx context.Context, _ json.RawMessage, _ prometheus.Registerer) error {
	return s.client.Open(ctx)
}

// Write implements querylog.Sink.
func (s *Sink) Write(batch []*querylog.Event) error {
	return s.WriteBatch(context.Background(), batch, querylog.PrivacyFull)
}

// WriteBatch inserts one batch as one request. The events carry their
// privacy transform already; the mode is not needed.
func (s *Sink) WriteBatch(ctx context.Context, batch []*querylog.Event, _ querylog.Mode) error {
	if len(batch) == 0 {
		return nil
	}
	s.body.Reset()
	enc := json.NewEncoder(&s.body)
	enc.SetEscapeHTML(false)
	for _, ev := range batch {
		// Encode ends every row with a newline, which JSONEachRow expects.
		if err := enc.Encode(record.From(ev)); err != nil {
			return fmt.Errorf("clickhouse: encode row: %w", err)
		}
	}
	return s.client.Post(ctx, s.body.Bytes())
}

// Flush implements querylog.Sink: nothing is buffered between batches.
func (s *Sink) Flush(context.Context) error { return nil }

// Close implements querylog.Sink.
func (s *Sink) Close() error {
	s.client.Close()
	return nil
}
