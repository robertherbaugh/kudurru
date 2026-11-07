package main

import (
	"context"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
)

// rootAnchorOutput never reacquires a publication. Each transfer continuation
// keeps this exact private component and the original request cancellation.
type rootAnchorOutput struct {
	ctx   context.Context
	seal  *recursiveRootSeal
	read  *recursiveRootRead
	state atomic.Uint32
}

const (
	anchorOutputReady uint32 = iota
	anchorOutputBusy
	anchorOutputWritten
	anchorOutputTerminal
)

func captureRootAnchorOutput(req *pipeline.Request) (dnsserver.Output, error) {
	seal, err := servingdns.Component[*recursiveRootSeal](req, recursiveRootComponent)
	if err != nil {
		return nil, err
	}
	read, err := seal.captureRead(req.Ctx)
	if err != nil {
		return nil, err
	}
	return &rootAnchorOutput{ctx: req.Ctx, seal: seal, read: read}, nil
}

// Emit holds mode/R only around the synchronous actual transport handoff.
func (o *rootAnchorOutput) Emit(fn func() error) error {
	if o == nil || fn == nil || !o.state.CompareAndSwap(anchorOutputReady, anchorOutputBusy) {
		return serving.ErrDependency
	}
	completed := false
	defer func() {
		// Panic or incomplete callback cannot mint another output attempt.
		if !completed {
			o.state.Store(anchorOutputTerminal)
		}
	}()
	err := o.seal.withRead(o.read, func(check func() error) error {
		if err := check(); err != nil {
			return err
		}
		return fn()
	})
	if err != nil {
		return err
	}
	completed = true
	o.state.Store(anchorOutputWritten)
	return nil
}

// Next precedes the next iterator pull. Failure never rescues an old frame.
func (o *rootAnchorOutput) Next() error {
	if o == nil || !o.state.CompareAndSwap(anchorOutputWritten, anchorOutputBusy) {
		return serving.ErrDependency
	}
	completed := false
	defer func() {
		if !completed {
			o.state.Store(anchorOutputTerminal)
		}
	}()
	read, err := o.seal.captureRead(o.ctx)
	if err != nil {
		return err
	}
	o.read = read
	completed = true
	o.state.Store(anchorOutputReady)
	return nil
}
