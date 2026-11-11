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
	read, err := seal.captureReadCurrent(req.Ctx, seal.dnsCurrent)
	if err != nil {
		return nil, err
	}
	return &rootAnchorInsertion{seal: seal, read: read}, nil
}

func (i *rootAnchorInsertion) Apply(fn func() error) error {
	if i == nil || fn == nil || !i.used.CompareAndSwap(false, true) {
		return serving.ErrDependency
	}
	// Raw response-cache work and native resolver mutations are checked under
	// the retained DNS policy and may return data while skipping a
	// superseded fill (D332, D377).
	return i.seal.withReadCurrent(i.read, i.seal.dnsCurrent, func(check func() error) error {
		if err := check(); err != nil {
			return err
		}
		// A held DNS lease may still finish after an ordinary canonical commit,
		// but its detached result cannot mutate the cache. Anchor retirement and
		// dependency loss remain errors, checked above under the original hold.
		if !i.seal.current() {
			return nil
		}
		return fn()
	})
}
