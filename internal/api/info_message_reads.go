package api

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// InfoMessageReadSource projects serving diagnoses from the original complete
// HTTP request lease at one observation time. It must stop on an emit error,
// never retain the callback, and reject missing, released, or canceled owners.
type InfoMessageReadSource interface {
	InfoMessages(context.Context, time.Time, func(severity, source, reason, object, message, hint string) error) error
}

const (
	maxInfoMessages     = 10_000
	maxInfoMessageBytes = 8 << 20
	maxInfoMessageText  = 64 << 10
)

// infoMessageCollector retains the first failure even if a producer incorrectly
// swallows it. No partial diagnostics response is successful.
type infoMessageCollector struct {
	node  string
	items []DiagnosisMessage
	bytes int
	err   error
}

func (c *infoMessageCollector) add(m DiagnosisMessage) error {
	if c.err != nil {
		return c.err
	}
	invalid := func() error {
		c.err = errUnavailable("diagnosis messages exceed bounds or contain invalid fields")
		return c.err
	}
	if len(c.items) >= maxInfoMessages || m.Severity != "error" && m.Severity != "warning" && m.Severity != "info" {
		return invalid()
	}
	fields := [...]struct {
		value string
		limit int
	}{{m.Severity, 7}, {m.Source, 256}, {m.Reason, 256}, {m.Object, 512}, {m.Message, maxInfoMessageText}, {m.Hint, 8 << 10}, {c.node, 256}}
	n := 0
	for _, f := range fields {
		if len(f.value) > f.limit || !utf8.ValidString(f.value) || strings.ContainsRune(f.value, 0) || len(f.value) > maxInfoMessageBytes-c.bytes-n {
			return invalid()
		}
		n += len(f.value)
	}
	// Strings may originate in an immutable image; detach only after preflight.
	m.Severity, m.Source, m.Reason = strings.Clone(m.Severity), strings.Clone(m.Source), strings.Clone(m.Reason)
	m.Object, m.Message, m.Hint, m.Node = strings.Clone(m.Object), strings.Clone(m.Message), strings.Clone(m.Hint), strings.Clone(c.node)
	c.bytes += n
	c.items = append(c.items, m)
	return nil
}

func (s *Server) capturedInfoMessages(ctx context.Context) (*messagesOutput, error) {
	if s.opts.InfoMessageReads == nil || ctx == nil || ctx.Err() != nil {
		return nil, errUnavailable("original diagnosis reader unavailable")
	}
	now := s.opts.Now()
	if now.IsZero() {
		return nil, errUnavailable("diagnosis observation clock unavailable")
	}
	out := infoMessageCollector{node: s.opts.NodeName, items: []DiagnosisMessage{}}
	err := s.opts.InfoMessageReads.InfoMessages(ctx, now, func(severity, source, reason, object, message, hint string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return out.add(DiagnosisMessage{Severity: severity, Source: source, Reason: reason, Object: object, Message: message, Hint: hint})
	})
	if err != nil || out.err != nil {
		return nil, errUnavailable("original diagnosis read: %v", firstInfoError(err, out.err))
	}
	if err := s.infoOperationalMessages(ctx, now, out.add); err != nil {
		return nil, errUnavailable("operational diagnosis read: %v", err)
	}
	slices.SortFunc(out.items, func(a, b DiagnosisMessage) int {
		for _, pair := range [][2]string{{a.Source, b.Source}, {a.Object, b.Object}, {a.Reason, b.Reason}, {a.Message, b.Message}, {a.Severity, b.Severity}, {a.Hint, b.Hint}} {
			if n := strings.Compare(pair[0], pair[1]); n != 0 {
				return n
			}
		}
		return 0
	})
	if err := ctx.Err(); err != nil {
		return nil, errUnavailable("original diagnosis context: %v", err)
	}
	return &messagesOutput{Body: newList(out.items, "")}, nil
}

func firstInfoError(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
