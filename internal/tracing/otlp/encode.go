// Package otlp exports kept traces to an OpenTelemetry collector with
// OTLP/HTTP in its binary protobuf encoding (D382). The request message,
// ExportTraceServiceRequest, is written by hand with protowire: the
// generated OTLP Go package registers the gRPC service and the gRPC
// gateway, which kudurru does not otherwise link, and the binary-size
// budget (D138) is better spent elsewhere. The encoding follows
// opentelemetry/proto/collector/trace/v1 and trace/v1 field for field;
// encode_test.go decodes it with the generated types.
package otlp

import (
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// Resource describes this process in every export.
type Resource struct {
	// ServiceName is service.name, "kudurru" by default.
	ServiceName string
	// Version is service.version; Instance is service.instance.id (the
	// node name).
	Version, Instance string
}

// scopeName is the instrumentation scope of every span.
const scopeName = "github.com/robertherbaugh/kudurru/internal/tracing"

// OTLP status codes.
const (
	statusOK    = 1
	statusError = 2
)

// AppendRequest appends the ExportTraceServiceRequest for traces to b.
func AppendRequest(b []byte, res Resource, traces []*tracing.Trace) []byte {
	// ExportTraceServiceRequest.resource_spans = 1: one ResourceSpans.
	return appendMessage(b, 1, func(b []byte) []byte {
		// ResourceSpans.resource = 1
		b = appendMessage(b, 1, func(b []byte) []byte {
			b = appendStrAttr(b, 1, "service.name", res.ServiceName)
			if res.Version != "" {
				b = appendStrAttr(b, 1, "service.version", res.Version)
			}
			if res.Instance != "" {
				b = appendStrAttr(b, 1, "service.instance.id", res.Instance)
			}
			return b
		})
		// ResourceSpans.scope_spans = 2
		return appendMessage(b, 2, func(b []byte) []byte {
			// ScopeSpans.scope = 1: InstrumentationScope{name = 1, version = 2}
			b = appendMessage(b, 1, func(b []byte) []byte {
				b = appendString(b, 1, scopeName)
				if res.Version != "" {
					b = appendString(b, 2, res.Version)
				}
				return b
			})
			for _, tr := range traces {
				id := tr.ID()
				for _, s := range tr.Spans() {
					// ScopeSpans.spans = 2
					b = appendMessage(b, 2, func(b []byte) []byte { return appendSpan(b, id, s) })
				}
			}
			return b
		})
	})
}

func appendSpan(b []byte, trace tracing.TraceID, s tracing.Span) []byte {
	b = appendBytes(b, 1, trace[:])
	b = appendBytes(b, 2, s.ID[:])
	if s.Parent.IsValid() {
		b = appendBytes(b, 4, s.Parent[:])
	}
	b = appendString(b, 5, s.Name)
	b = protowire.AppendTag(b, 6, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(s.Kind))
	b = protowire.AppendTag(b, 7, protowire.Fixed64Type)
	b = protowire.AppendFixed64(b, uint64(s.Start.UnixNano()))
	b = protowire.AppendTag(b, 8, protowire.Fixed64Type)
	b = protowire.AppendFixed64(b, uint64(s.End.UnixNano()))
	for _, a := range s.Attrs {
		if a.IsInt {
			b = appendIntAttr(b, 9, a.Key, a.Int)
		} else {
			b = appendStrAttr(b, 9, a.Key, a.Str)
		}
	}
	// Status = 15: {message = 2, code = 3}
	return appendMessage(b, 15, func(b []byte) []byte {
		if s.Error {
			if s.Message != "" {
				b = appendString(b, 2, s.Message)
			}
			b = protowire.AppendTag(b, 3, protowire.VarintType)
			return protowire.AppendVarint(b, statusError)
		}
		if s.Kind == tracing.KindServer {
			b = protowire.AppendTag(b, 3, protowire.VarintType)
			return protowire.AppendVarint(b, statusOK)
		}
		return b
	})
}

// appendStrAttr appends a KeyValue{key = 1, value = 2: AnyValue{string_value = 1}}.
func appendStrAttr(b []byte, field protowire.Number, key, value string) []byte {
	return appendMessage(b, field, func(b []byte) []byte {
		b = appendString(b, 1, key)
		return appendMessage(b, 2, func(b []byte) []byte { return appendString(b, 1, value) })
	})
}

// appendIntAttr appends a KeyValue whose AnyValue is int_value = 3.
func appendIntAttr(b []byte, field protowire.Number, key string, value int64) []byte {
	return appendMessage(b, field, func(b []byte) []byte {
		b = appendString(b, 1, key)
		return appendMessage(b, 2, func(b []byte) []byte {
			b = protowire.AppendTag(b, 3, protowire.VarintType)
			return protowire.AppendVarint(b, uint64(value)) //nolint:gosec // protobuf int64 is the two's complement varint
		})
	})
}

func appendString(b []byte, field protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, field, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func appendBytes(b []byte, field protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, field, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

// appendMessage appends a length-delimited embedded message that body
// writes. The length is not known in advance, so body writes after a
// placeholder and the result is moved into place: the messages are small
// and this runs off the query path.
func appendMessage(b []byte, field protowire.Number, body func([]byte) []byte) []byte {
	b = protowire.AppendTag(b, field, protowire.BytesType)
	start := len(b)
	b = body(b)
	n := len(b) - start
	size := protowire.SizeVarint(uint64(n)) //nolint:gosec // a length, never negative
	b = append(b, make([]byte, size)...)
	copy(b[start+size:], b[start:start+n])
	protowire.AppendVarint(b[start:start], uint64(n)) //nolint:gosec // a length, never negative
	return b
}
