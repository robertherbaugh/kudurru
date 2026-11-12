package config

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Tracing configures OpenTelemetry tracing of DNS queries (D382,
// dev-docs/design-record.md §11.5). Every key is host-only: the collector
// address and its credential are the operator's, never an API value.
type Tracing struct {
	// Enabled turns tracing on. Off, no query carries a trace and nothing
	// is exported; TRACEPARENT options are then neither read nor removed
	// (no upstream is ever sent client EDNS options either way).
	Enabled bool `koanf:"enabled" json:"enabled"`
	// Endpoint is the OTLP/HTTP traces URL of the collector.
	Endpoint string `koanf:"endpoint" json:"endpoint"`
	// SampleRate is the head sample in traces per million queries that do
	// not continue an edge peer's trace (1000 = 0.1 %).
	SampleRate int `koanf:"sampleRate" json:"sampleRate"`
	// SlowThreshold keeps every query at least this slow; 0 turns the
	// slow rule off.
	SlowThreshold Duration `koanf:"slowThreshold" json:"slowThreshold"`
	// AcceptTraceparent continues the trace of an authenticated edge peer
	// (PROXYv2 with the shared secret) from its TRACEPARENT option.
	AcceptTraceparent bool `koanf:"acceptTraceparent" json:"acceptTraceparent"`
	// EDNSOptionCode is the TRACEPARENT option code, 65001-65534.
	EDNSOptionCode int `koanf:"ednsOptionCode" json:"ednsOptionCode"`
	// ServiceName is the service.name resource attribute.
	ServiceName string `koanf:"serviceName" json:"serviceName"`
	// Headers are extra, non-secret request headers.
	Headers map[string]string `koanf:"headers" json:"headers,omitempty"`
	// BearerTokenRef and CARef name the collector credential and a private
	// CA bundle (file or env).
	// An empty reference is unset.
	BearerTokenRef TracingRef `koanf:"bearerTokenRef" json:"bearerTokenRef"`
	CARef          TracingRef `koanf:"caRef" json:"caRef"`
	// Compression is gzip or none.
	Compression string `koanf:"compression" json:"compression"`
	// QueueSize bounds the kept traces waiting for export.
	QueueSize int `koanf:"queueSize" json:"queueSize"`
	// Timeout bounds one export request.
	Timeout Duration `koanf:"timeout" json:"timeout"`
}

// Defaults and bounds of tracing.* (D382).
const (
	DefaultTracingSampleRate = 1000
	MaxTracingSampleRate     = 1_000_000
	DefaultTracingSlow       = 250 * time.Millisecond
	DefaultTracingOptionCode = 65500
	DefaultTracingQueueSize  = 2048
	MaxTracingQueueSize      = 65536
	DefaultTracingTimeout    = 10 * time.Second
	maxTracingHeaders        = 16
)

// validate checks what the shapes cannot.
func (t *Tracing) validate() []error {
	var errs []error
	if t.Enabled && t.Endpoint == "" {
		errs = append(errs, &FieldError{Path: "tracing.endpoint", Message: "is required when tracing.enabled is true"})
	}
	if t.Endpoint != "" {
		if _, err := ParseSinkURL(t.Endpoint); err != nil {
			errs = append(errs, fmt.Errorf("tracing.endpoint %w", err))
		}
	}
	if t.EDNSOptionCode < 65001 || t.EDNSOptionCode > 65534 {
		errs = append(errs, fmt.Errorf("tracing.ednsOptionCode: use a code of the local range 65001-65534 (dnsdist and PowerDNS Recursor use 65500)"))
	}
	if t.SlowThreshold < 0 || t.Timeout < Duration(time.Second) {
		errs = append(errs, fmt.Errorf("tracing.slowThreshold must not be negative and tracing.timeout must be at least 1s"))
	}
	if len(t.Headers) > maxTracingHeaders {
		errs = append(errs, fmt.Errorf("tracing.headers: at most %d headers", maxTracingHeaders))
	}
	for name, value := range t.Headers {
		switch {
		case name == "" || strings.ContainsFunc(name, func(r rune) bool { return r <= ' ' || r >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?={}", r) }):
			errs = append(errs, fmt.Errorf("tracing.headers: %q is not a header name", name))
		case http.CanonicalHeaderKey(name) == "Authorization":
			errs = append(errs, fmt.Errorf("tracing.headers: set the credential with tracing.bearerTokenRef; header values are settings, not secrets"))
		case strings.ContainsFunc(value, func(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }):
			// The message never repeats the value.
			errs = append(errs, fmt.Errorf("tracing.headers[%s]: the value holds a control character", name))
		}
	}
	for at, ref := range map[string]TracingRef{"tracing.bearerTokenRef": t.BearerTokenRef, "tracing.caRef": t.CARef} {
		if r := ref.Ref(); r != nil {
			errs = append(errs, r.validate(at)...)
		}
	}
	return errs
}

// TracingRef names a credential tracing reads: a file or an environment
// variable, through the node's credential grants. On Kubernetes a Secret is
// mounted as one of the two.
type TracingRef struct {
	File string `koanf:"file" json:"file,omitempty"`
	Env  string `koanf:"env" json:"env,omitempty"`
}

// Ref returns the reference, or nil when it names nothing.
func (r TracingRef) Ref() *CredentialRef {
	if r.File == "" && r.Env == "" {
		return nil
	}
	return &CredentialRef{File: r.File, Env: r.Env}
}
