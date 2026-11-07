package main

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
)

type rootNativeSource struct {
	ctx      context.Context
	deadline time.Time
	seal     *recursiveRootSeal
}

func captureRootNativeSource(req *pipeline.Request) (recursor.CacheMutationSource, error) {
	seal, err := servingdns.Component[*recursiveRootSeal](req, recursiveRootComponent)
	if err != nil {
		return nil, err
	}
	// Validate the actual Store's original finite source before recursion. Each
	// acquisition gets a separate attempt under its own earlier child deadline.
	if _, err := seal.captureRead(req.Ctx); err != nil {
		return nil, err
	}
	deadline, _ := req.Ctx.Deadline()
	return &rootNativeSource{ctx: req.Ctx, deadline: deadline, seal: seal}, nil
}

func (s *rootNativeSource) Domain() [32]byte {
	if s.seal.managed != nil {
		return s.seal.managed.CacheDomain()
	}
	return [32]byte{}
}
func (s *rootNativeSource) check() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(s.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
func (s *rootNativeSource) Capture(ctx context.Context) (recursor.CacheMutation, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok || deadline.After(s.deadline) {
		return nil, serving.ErrDependency
	}
	read, err := s.seal.captureRead(ctx)
	if err != nil {
		return nil, err
	}
	return &rootNativeMutation{source: s, insertion: rootAnchorInsertion{seal: s.seal, read: read}}, nil
}

type rootNativeMutation struct {
	source    *rootNativeSource
	insertion rootAnchorInsertion
}

func (m *rootNativeMutation) Apply(fn func() error) error {
	return m.insertion.Apply(func() error {
		if err := m.source.check(); err != nil {
			return err
		}
		if err := fn(); err != nil {
			return err
		}
		return m.source.check()
	})
}
