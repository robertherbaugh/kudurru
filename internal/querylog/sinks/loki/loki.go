// Package loki is the Grafana Loki sink of querylog.sinks (D371): each
// batch is one push to /loki/api/v1/push in the JSON format, sent with the
// standard library client (internal/querylog/sinks/httpbatch).
//
// Streams carry only labels with few values: the fixed labels of the
// settings, the node and the status class. Everything that differs per
// query travels in the log line, which is the JSON record of
// internal/querylog/sinks/record, and (unless turned off) as structured
// metadata: client, client_name, domain, qtype, status and trace_id.
// docs/deploy/query-log-sinks.md documents the mapping.
package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/httpbatch"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/record"
)

// PushPath is Loki's push endpoint.
const PushPath = "/loki/api/v1/push"

// TenantHeader selects the tenant of a multi-tenant Loki.
const TenantHeader = "X-Scope-OrgID"

// Options are the node's own inputs to every Loki sink.
type Options struct {
	// Resolve reads credential references.
	Resolve httpbatch.Resolver
	Logger  *slog.Logger
	// UserAgent is sent with every request.
	UserAgent string
}

// Sink pushes batches to one Loki.
type Sink struct {
	client   *httpbatch.Client
	labels   map[string]string
	metadata bool
	line     bytes.Buffer
}

// New builds the sink of one querylog.sinks entry from its loki object. It
// validates the settings and does no I/O.
func New(spec querylog.SinkSpec, o Options) (*Sink, error) {
	var settings config.QuerylogLoki
	if len(spec.Config) == 0 {
		return nil, errors.New("the loki settings are missing")
	}
	if err := json.Unmarshal(spec.Config, &settings); err != nil {
		return nil, errors.New("the loki settings are malformed")
	}
	if settings.URL == "" {
		return nil, errors.New("loki.url is required")
	}
	if errs := settings.Validate(config.QuerylogSinkLoki); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if !config.ValidCompression(settings.Compression) {
		return nil, errors.New("loki.compression is not gzip or none")
	}
	target, err := PushURL(settings)
	if err != nil {
		return nil, err
	}
	header := http.Header{"Content-Type": {"application/json"}}
	if settings.Tenant != "" {
		header.Set(TenantHeader, settings.Tenant)
	}
	if o.UserAgent != "" {
		header.Set("User-Agent", o.UserAgent)
	}
	client, err := httpbatch.New(httpbatch.Options{
		Destination: "Loki", URL: target, Header: header,
		Username: settings.Username, Password: settings.PasswordRef, Bearer: settings.BearerTokenRef, CA: settings.CARef,
		Gzip: config.GzipBody(settings.Compression), Resolve: o.Resolve, Logger: o.Logger,
	})
	if err != nil {
		return nil, err
	}
	return &Sink{client: client, labels: settings.StreamLabels(), metadata: settings.UsesStructuredMetadata()}, nil
}

// PushURL is the request URL: the base URL with the push path, unless the
// settings already name the push endpoint.
func PushURL(settings config.QuerylogLoki) (*url.URL, error) {
	base, err := config.ParseSinkURL(settings.URL)
	if err != nil {
		return nil, fmt.Errorf("loki.url %w", err)
	}
	target := *base
	if path := strings.TrimSuffix(target.Path, "/"); !strings.HasSuffix(path, PushPath) {
		target.Path = path + PushPath
	} else {
		target.Path = path
	}
	return &target, nil
}

// push is the body of one request.
type push struct {
	Streams []*stream `json:"streams"`
}

type stream struct {
	Stream map[string]string `json:"stream"`
	Values []entry           `json:"values"`
}

// entry is one log line: [timestamp, line] or [timestamp, line, metadata].
type entry struct {
	// timestamp is Unix nanoseconds as a decimal string.
	timestamp string
	line      string
	metadata  map[string]string
}

// MarshalJSON renders the entry as the array Loki expects.
func (e entry) MarshalJSON() ([]byte, error) {
	if len(e.metadata) == 0 {
		return json.Marshal([2]string{e.timestamp, e.line})
	}
	return json.Marshal([3]any{e.timestamp, e.line, e.metadata})
}

// Name implements querylog.Sink.
func (s *Sink) Name() string { return config.QuerylogSinkLoki }

// Open reads the credential references. It does not contact Loki.
func (s *Sink) Open(ctx context.Context, _ json.RawMessage, _ prometheus.Registerer) error {
	return s.client.Open(ctx)
}

// Write implements querylog.Sink.
func (s *Sink) Write(batch []*querylog.Event) error {
	return s.WriteBatch(context.Background(), batch, querylog.PrivacyFull)
}

// WriteBatch pushes one batch as one request. The events carry their
// privacy transform already; the mode is not needed.
func (s *Sink) WriteBatch(ctx context.Context, batch []*querylog.Event, _ querylog.Mode) error {
	if len(batch) == 0 {
		return nil
	}
	body, err := s.encode(batch)
	if err != nil {
		return err
	}
	return s.client.Post(ctx, body)
}

// encode groups the batch into streams by node and status class, in the
// order they first appear, and keeps the order of the events inside one.
func (s *Sink) encode(batch []*querylog.Event) ([]byte, error) {
	var body push
	streams := map[string]*stream{}
	enc := json.NewEncoder(&s.line)
	enc.SetEscapeHTML(false)
	for _, ev := range batch {
		rec := record.From(ev)
		key := rec.Node + "\x00" + rec.StatusClass
		st := streams[key]
		if st == nil {
			labels := make(map[string]string, len(s.labels)+2)
			for name, value := range s.labels {
				labels[name] = value
			}
			if rec.Node != "" {
				labels[config.QuerylogLokiLabelNode] = rec.Node
			}
			labels[config.QuerylogLokiLabelStatusClass] = rec.StatusClass
			st = &stream{Stream: labels}
			streams[key] = st
			body.Streams = append(body.Streams, st)
		}
		s.line.Reset()
		if err := enc.Encode(rec); err != nil {
			return nil, fmt.Errorf("loki: encode line: %w", err)
		}
		e := entry{timestamp: strconv.FormatInt(ev.Time.UnixNano(), 10), line: strings.TrimSuffix(s.line.String(), "\n")}
		if s.metadata {
			e.metadata = metadata(rec)
		}
		st.Values = append(st.Values, e)
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("loki: encode push: %w", err)
	}
	return out, nil
}

// metadata is the structured metadata of one line: the per-query values an
// operator filters on without parsing the line. Empty values are left out.
func metadata(rec record.Record) map[string]string {
	out := make(map[string]string, 6)
	for name, value := range map[string]string{
		"client": rec.Client, "client_name": rec.ClientName, "domain": rec.Domain,
		"qtype": rec.QType, "status": rec.Status, "trace_id": rec.TraceID,
	} {
		if value != "" {
			out[name] = value
		}
	}
	return out
}

// Flush implements querylog.Sink: nothing is buffered between batches.
func (s *Sink) Flush(context.Context) error { return nil }

// Close implements querylog.Sink.
func (s *Sink) Close() error {
	s.client.Close()
	return nil
}
