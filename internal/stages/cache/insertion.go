package cache

import "github.com/robertherbaugh/kudurru/internal/pipeline"

// Insertion owns one original, finite cache mutation attempt. Apply consumes
// the attempt and holds admission through fn's synchronous cache mutation.
// It must not acquire resources, renew the publication, or release its lease.
type Insertion interface {
	Apply(fn func() error) error
}

func (s *Stage) captureInsertion(req *pipeline.Request) (Insertion, error) {
	if s.CaptureInsertion == nil {
		return nil, nil
	}
	insertion, err := s.CaptureInsertion(req)
	if err != nil {
		return nil, err
	}
	if insertion == nil {
		return nil, pipeline.ErrServingCapture
	}
	return insertion, nil
}

func applyInsertion(insertion Insertion, fn func() error) error {
	if insertion == nil {
		return fn()
	}
	return insertion.Apply(fn)
}

func (s *Stage) rebind(req *pipeline.Request, view *View, scope pipeline.CacheScope) error {
	// A dump is data, never an installed managed owner/local registration.
	if view.managed {
		return nil
	}
	if _, loaded := s.rebound.LoadOrStore(scope, true); loaded {
		return nil
	}
	insertion, err := s.captureInsertion(req)
	if err == nil {
		err = applyInsertion(insertion, func() error {
			if !view.current() {
				// Currency fences mutation, not library callers' held reads.
				// Root admission independently rejects its retired source.
				return nil
			}
			s.c.RebindRestored(scope)
			return nil
		})
	}
	if err != nil {
		s.rebound.Delete(scope)
	}
	return err
}
