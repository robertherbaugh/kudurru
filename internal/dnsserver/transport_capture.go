package dnsserver

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type transportRequestKey struct{}

// captureTransport owns the only complete capture, including unsigned queries.
// A handler must keep cleanup through emission, even if the pipeline never runs.
func (h *Handler) captureTransport(ctx context.Context, msg *dns.Msg, client pipeline.Client, received time.Time) (*pipeline.Request, *StaticKeyring, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if received.IsZero() {
		received = time.Now()
	}
	ctx, cancel := context.WithDeadline(ctx, received.Add(h.timeout()))
	deadline, _ := ctx.Deadline() // retain an earlier original transport deadline
	req := &pipeline.Request{Ctx: ctx, Msg: msg, Received: received, Deadline: deadline, Client: client}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			cancel()
			if req.Serving != nil {
				req.Serving.Release()
				req.Serving = nil
			}
		})
	}
	if h.Capture == nil || h.Keys == nil {
		return req, nil, cleanup, pipeline.ErrServingCapture
	}
	lease, err := h.Capture(req)
	req.Serving = lease
	if err != nil {
		return req, nil, cleanup, err
	}
	if lease == nil {
		return req, nil, cleanup, pipeline.ErrServingCapture
	}
	if h.Output != nil {
		output, err := h.Output(req)
		if err != nil {
			return req, nil, cleanup, err
		}
		if output == nil {
			return req, nil, cleanup, pipeline.ErrServingCapture
		}
		ctx = context.WithValue(ctx, transportOutputKey{}, output)
		req.Ctx = ctx
	}
	keys, err := h.Keys(req)
	if err != nil || keys == nil {
		return req, nil, cleanup, pipeline.ErrServingCapture
	}
	if err := ctx.Err(); err != nil {
		return req, nil, cleanup, err
	}
	ctx = context.WithValue(ctx, transportResponseKey{}, &transportResponse{})
	req.Ctx = context.WithValue(ctx, transportRequestKey{}, req)
	return req, keys, cleanup, nil
}

var errOriginalDeadline = errors.New("dnsserver: original request deadline expired")
