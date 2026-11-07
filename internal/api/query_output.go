package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// DNSQueryEmission owns one original output attempt. Emit consumes it while
// fn synchronously writes and flushes the response. It never recaptures input.
type DNSQueryEmission interface {
	Emit(fn func() error) error
}

type dnsQueryHandoffKey struct{}

type dnsQueryHandoff struct {
	ctx      context.Context
	cancel   context.CancelFunc
	lease    pipeline.ServingLease
	response *pipeline.Response
	emission DNSQueryEmission
}

func (h *dnsQueryHandoff) close() {
	defer func() {
		if h.cancel != nil {
			h.cancel()
		}
	}()
	defer func() {
		if h.lease != nil {
			h.lease.Release()
		}
	}()
	h.response.Close()
}

// Huma retains the original typed output and all normal transformations. Only
// this operation's bytes/status/headers are held until final root admission.
type queryHumaContext = huma.Context

type dnsQueryBuffer struct {
	queryHumaContext
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

const maxDNSQueryOutputBytes = 4 << 20

func (b *dnsQueryBuffer) Unwrap() huma.Context            { return b.queryHumaContext }
func (b *dnsQueryBuffer) SetStatus(code int)              { b.status = code }
func (b *dnsQueryBuffer) Status() int                     { return b.status }
func (b *dnsQueryBuffer) SetHeader(name, value string)    { b.header.Set(name, value) }
func (b *dnsQueryBuffer) AppendHeader(name, value string) { b.header.Add(name, value) }
func (b *dnsQueryBuffer) BodyWriter() io.Writer           { return b }
func (b *dnsQueryBuffer) Write(data []byte) (int, error) {
	if b.err != nil {
		return len(data), nil
	}
	if len(data) > maxDNSQueryOutputBytes-b.body.Len() {
		// Huma panics on serialization write errors. Drain overflow without
		// storing it; the middleware refuses before touching the transport.
		b.err = errors.New("DNS diagnostic response exceeds output bound")
		return len(data), nil
	}
	return b.body.Write(data)
}

func (s *Server) dnsQueryEmissionMW(ctx huma.Context, next func(huma.Context)) {
	handoff := &dnsQueryHandoff{}
	defer handoff.close()
	b := &dnsQueryBuffer{queryHumaContext: huma.WithContext(ctx, context.WithValue(ctx.Context(), dnsQueryHandoffKey{}, handoff)), header: make(http.Header), status: http.StatusOK}
	next(b)
	r, w := humago.Unwrap(ctx)
	refuse := func() { writeError(w, r, errUnavailable("the original DNS output generation is unavailable")) }
	if b.err != nil {
		refuse()
		return
	}
	write := func() error {
		for name, values := range b.header {
			w.Header()[name] = append([]string(nil), values...)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(b.body.Len()))
		w.WriteHeader(b.status)
		n, err := w.Write(b.body.Bytes())
		if err == nil && n != b.body.Len() {
			err = io.ErrShortWrite
		}
		return err
	}
	// Validation/authentication refusals contain no DNS answer and need no
	// answer admission. A successful guarded output always has its own attempt.
	if b.status >= 400 {
		if err := write(); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if handoff.emission == nil || handoff.ctx == nil {
		refuse()
		return
	}
	deadline, finite := handoff.ctx.Deadline()
	if handoff.response != nil {
		if expiry := handoff.response.Security.ValidUntil; !expiry.IsZero() && expiry.Before(deadline) {
			deadline = expiry
		}
	}
	if !finite || handoff.ctx.Err() != nil || !time.Now().Before(deadline) {
		refuse()
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(deadline); err != nil {
		refuse()
		return
	}
	started, completed := false, false
	err := handoff.emission.Emit(func() error {
		if err := handoff.ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) || handoff.response != nil && !handoff.response.Security.AvailableAt(time.Now()) {
			return context.DeadlineExceeded
		}
		stopped := make(chan struct{})
		stop := context.AfterFunc(handoff.ctx, func() { defer close(stopped); _ = rc.SetWriteDeadline(time.Now()) })
		defer func() {
			if !stop() {
				<-stopped
			}
		}()
		started = true
		if err := write(); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return err
		}
		completed = true
		return nil
	})
	if completed {
		return
	} // a later scope error cannot reset an actual flush
	if started {
		panic(http.ErrAbortHandler)
	}
	if err != nil {
		refuse()
		return
	}
	refuse() // a callback that never ran did not emit the response
}
