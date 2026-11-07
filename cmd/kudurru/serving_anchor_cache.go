package main

import (
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
)

type rootAnchorInsertion struct {
	seal *recursiveRootSeal
	read *recursiveRootRead
	used atomic.Bool
}

// Detached work supplies its own original finite context before acquisition;
// its cloned lease retains this exact component, never a successor publication.
func captureRootAnchorInsertion(req *pipeline.Request) (cachestage.Insertion, error) {
	seal, err := servingdns.Component[*recursiveRootSeal](req, recursiveRootComponent)
	if err != nil {
		return nil, err
	}
	read, err := seal.captureRead(req.Ctx)
	if err != nil {
		return nil, err
	}
	return &rootAnchorInsertion{seal: seal, read: read}, nil
}

func (i *rootAnchorInsertion) Apply(fn func() error) error {
	if i == nil || fn == nil || !i.used.CompareAndSwap(false, true) {
		return serving.ErrDependency
	}
	return i.seal.withRead(i.read, func(check func() error) error {
		if err := check(); err != nil {
			return err
		}
		return fn()
	})
}
