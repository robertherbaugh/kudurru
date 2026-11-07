package recursor

import (
	"context"
	"errors"
	"time"
)

// CacheMutation is one pre-acquisition attempt. Apply consumes it and holds
// admission through synchronous cache writes; no fetching or release belongs
// inside the callback.
type CacheMutation interface{ Apply(func() error) error }

// CacheMutationSource retains one original serving component and finite query
// lifetime. Capture issues independent attempts before each acquisition. It
// must never find a replacement publication or widen the original lifetime.
type CacheMutationSource interface {
	Domain() [32]byte
	Capture(context.Context) (CacheMutation, error)
}

type cacheMutationKey struct{}

// WithCacheMutations binds the trusted serving caller's original source. Child
// upstream contexts inherit it; the resolver never obtains a source itself.
func WithCacheMutations(ctx context.Context, source CacheMutationSource) context.Context {
	return context.WithValue(ctx, cacheMutationKey{}, source)
}

// ErrCacheAdmission means the original cache mutation authority is unavailable.
var ErrCacheAdmission = errors.New("recursor: original cache mutation admission unavailable")

func (r *Resolver) mutationSource(ctx context.Context) (CacheMutationSource, error) {
	source, _ := ctx.Value(cacheMutationKey{}).(CacheMutationSource)
	if source == nil {
		if r.requireCacheMutations {
			return nil, ErrCacheAdmission
		}
		return nil, nil
	}
	if source.Domain() != r.trustDomain {
		return nil, ErrCacheAdmission
	}
	return source, nil
}

type cacheMutationBatch struct {
	q        *query
	attempt  CacheMutation
	writes   []func()
	finished bool
}

func (q *query) captureMutations() (*cacheMutationBatch, error) {
	if q.st.cacheErr != nil {
		return nil, q.st.cacheErr
	}
	if q.source == nil {
		return nil, nil
	}
	attempt, err := q.source.Capture(q.ctx)
	if err == nil && attempt == nil {
		err = ErrCacheAdmission
	}
	if err != nil {
		q.st.cacheErr = err
		return nil, err
	}
	return &cacheMutationBatch{q: q, attempt: attempt}, nil
}

func (q *query) queueMutation(batch *cacheMutationBatch, fn func()) {
	if batch == nil {
		if q.source != nil || q.r.requireCacheMutations {
			q.st.cacheErr = ErrCacheAdmission
			return
		}
		fn()
		return
	}
	if batch.q != q || batch.finished || len(batch.writes) >= maxCNAME+2 {
		q.st.cacheErr = ErrCacheAdmission
		return
	}
	batch.writes = append(batch.writes, fn)
}

// commit is called once at the acquisition/response boundary, after all
// validation, copies and nested lookups have completed. No native lock is held.
func (b *cacheMutationBatch) commit() {
	if b == nil {
		return
	}
	if b.finished {
		b.q.st.cacheErr = ErrCacheAdmission
		return
	}
	b.finished = true
	if len(b.writes) == 0 {
		return
	}
	q := b.q
	err := b.attempt.Apply(func() error {
		check := func() error {
			if err := q.ctx.Err(); err != nil {
				return err
			}
			if deadline, ok := q.ctx.Deadline(); !ok || !time.Now().Before(deadline) {
				return context.DeadlineExceeded
			}
			if q.st.cacheErr != nil {
				return q.st.cacheErr
			}
			now := q.now()
			if now.Before(q.st.observed) || !q.st.exceptionUntil.IsZero() && !now.Before(q.st.exceptionUntil) {
				return ErrCacheAdmission
			}
			return nil
		}
		for _, write := range b.writes {
			if err := check(); err != nil {
				return err
			}
			write()
		}
		return check()
	})
	if err != nil && q.st.cacheErr == nil {
		q.st.cacheErr = err
	}
	b.writes = nil
}

func (q *query) queueCacheSet(batch *cacheMutationBatch, key string, value any, ttl time.Duration, origin time.Time) {
	expires := cacheExpiry(ttl, origin)
	q.queueMutation(batch, func() {
		q.r.cache.setUntil(key, value, expires, q.st.cacheEpoch, func() bool {
			now := q.now()
			return q.mutationAlive(now) && !now.Before(origin) && now.Before(expires)
		})
	})
}

// mutationAlive also checks the original wall deadline: a socket/lock wait can
// cross it just before the context timer delivers cancellation.
func (q *query) mutationAlive(now time.Time) bool {
	if q.ctx.Err() != nil || now.Before(q.st.observed) {
		return false
	}
	deadline, finite := q.ctx.Deadline()
	if q.source != nil && !finite || finite && !time.Now().Before(deadline) {
		return false
	}
	return q.st.exceptionUntil.IsZero() || now.Before(q.st.exceptionUntil)
}
