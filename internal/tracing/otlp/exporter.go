package otlp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/httpbatch"
	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// Options configures an Exporter.
type Options struct {
	// URL is the OTLP/HTTP traces endpoint, for example
	// http://collector:4318/v1/traces.
	URL      *url.URL
	Resource Resource
	// Headers are sent with every request (non-secret values only).
	Headers map[string]string
	// Bearer and CA are credential references (file or env), read through
	// Resolve.
	Bearer  *config.CredentialRef
	CA      *config.CredentialRef
	Gzip    bool
	Resolve httpbatch.Resolver
	Logger  *slog.Logger
}

// Exporter posts each batch once to the collector. It reuses the bounded
// HTTP client of the query-log batch sinks (one connection per request,
// no redirects, the credential never logged). A batch the collector
// refused or did not answer is dropped and counted by the tracer; it is
// never sent again, so a collector outage costs traces, never memory or
// answer time.
type Exporter struct {
	client *httpbatch.Client
	res    Resource

	mu   sync.Mutex
	open bool
	buf  []byte
}

// New checks the options; it does no I/O.
func New(o Options) (*Exporter, error) {
	header := http.Header{}
	header.Set("Content-Type", "application/x-protobuf")
	for k, v := range o.Headers {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			return nil, errors.New("otlp: set the Authorization header with tracing.bearer, not tracing.headers")
		}
		header.Set(k, v)
	}
	header.Set("User-Agent", "kudurru/"+o.Resource.Version)
	client, err := httpbatch.New(httpbatch.Options{
		Destination: "OTLP collector", URL: o.URL, Header: header,
		Bearer: o.Bearer, CA: o.CA, Gzip: o.Gzip, Resolve: o.Resolve, Logger: o.Logger,
	})
	if err != nil {
		return nil, err
	}
	if o.Resource.ServiceName == "" {
		o.Resource.ServiceName = "kudurru"
	}
	return &Exporter{client: client, res: o.Resource}, nil
}

// Export implements tracing.Exporter.
func (e *Exporter) Export(ctx context.Context, traces []*tracing.Trace) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.open {
		// The credential and CA are read on first use and again after a
		// failed read, so a collector or a file that is late at start
		// costs the first batches, not the process.
		if err := e.client.Open(ctx); err != nil {
			return err
		}
		e.open = true
	}
	e.buf = AppendRequest(e.buf[:0], e.res, traces)
	return e.client.Post(ctx, e.buf)
}

// Close releases the connection and forgets the credential.
func (e *Exporter) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.client.Close()
	e.open = false
}
