package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// requestItems is the state of ONE public request that runs several current
// commands, one for each of its items: POST /apply and POST /{kind}:batchDelete
// (D378). The original credential, lease and lifetime stay those of the
// request. What a single command cannot do is admit a second command after the
// request's own first commit, because that commit retires the request's
// original lease. The chain replaces that one comparison for the later items.
type requestItems struct {
	chain *providercoordinator.Chain
	pace  itemPace

	mu     sync.Mutex
	bound  bool
	writer *primaryWriterTerm
	target serving.TermActivation
	paced  time.Time // when the node last had a current publication for this request
}

// itemPace bounds how long one multi-item request may keep the node without a
// current publication (D378). Every commit makes the publication that DNS
// answers for local data and every other API request are bound to stale until
// the node has published again, and the items of one request commit back to
// back. After work of the request's items, the next item first waits, inside
// the request's lifetime, until the node has published everything committed
// so far, and then commits nothing for rest so that DNS clients and other
// callers are served from that publication. The wait itself is bounded by
// wait: when the node takes longer to publish (a large data set or a loaded
// machine), the item goes on without the publication, so that the pace never
// makes a request more than about twice as slow and cannot by itself run a
// request out of its lifetime.
type itemPace struct {
	work time.Duration
	rest time.Duration
	wait time.Duration
}

// defaultItemPace keeps a long request's local DNS answers and other callers
// unavailable for about two seconds at a time, plus the node's own time to
// publish, instead of for the whole request. A request that ends within two
// seconds never waits. A shorter work budget costs a long request far more:
// the node's time to publish grows with the data it serves (measured on 150
// new record sets without the race detector: 3.9 s unpaced, 26 s with a
// half-second budget, and waits from 0.2 s to 1.5 s).
var defaultItemPace = itemPace{work: 2 * time.Second, rest: 200 * time.Millisecond, wait: 2 * time.Second}

type requestItemsKey struct{}

// ObjectItems begins the item sequence of a multi-item public request. It
// captures nothing and admits nothing: every item still runs ObjectCommand
// with the request's original context values.
func (s *servingRuntime) ObjectItems(ctx context.Context) context.Context {
	if s == nil || ctx == nil || requestItemsFrom(ctx) != nil {
		return ctx
	}
	pace := defaultItemPace
	if s.itemPace != nil {
		pace = *s.itemPace
	}
	return context.WithValue(ctx, requestItemsKey{}, &requestItems{chain: s.providers.NewChain(), pace: pace})
}

func requestItemsFrom(ctx context.Context) *requestItems {
	items, _ := ctx.Value(requestItemsKey{}).(*requestItems)
	return items
}

// bind keeps every item of the request in the elected writer episode and the
// authority incarnation of its first item. A later election is not adopted.
func (i *requestItems) bind(writer *primaryWriterTerm, target serving.TermActivation) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.bound {
		// The first item is admitted only on a current publication.
		i.bound, i.writer, i.target, i.paced = true, writer, target, time.Now()
		return nil
	}
	if i.writer != writer || !i.target.Term.Same(target.Term) || i.target.Incarnation != target.Incarnation {
		return replication.ErrFenced
	}
	return nil
}

// due reports whether a later item must first let the node publish.
func (i *requestItems) due() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return time.Since(i.paced) >= i.pace.work
}

// published records that the node had a current publication for this
// request at the moment the wait returned.
func (i *requestItems) published() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.paced = time.Now()
}

// paceLaterItem lets the node publish the request's earlier items before a
// later item when the request has worked for its pace, and then keeps that
// publication current for the pace's rest. It never captures a new lease and
// admits nothing: the item is still admitted only by the chain's compare, so a
// command of another caller that commits while the request waits stops every
// later item. request is the request's own context, which carries its
// lifetime; bound additionally ends with the request's writer episode.
func (s *servingRuntime) paceLaterItem(request, bound context.Context, items *requestItems, target serving.TermActivation) error {
	if !items.due() {
		return nil
	}
	wait, cancel := bound, context.CancelFunc(func() {})
	if items.pace.wait > 0 {
		wait, cancel = context.WithTimeout(bound, items.pace.wait)
	}
	err := s.publisher.WaitTerm(wait, target)
	cancel()
	if err != nil && bound.Err() == nil && wait.Err() != nil {
		// The node did not publish within the wait's bound: go on without
		// the publication, and wait again after the next work budget.
		items.published()
		return nil
	}
	if err == nil {
		items.published()
		if items.pace.rest <= 0 {
			return nil
		}
		timer := time.NewTimer(items.pace.rest)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-bound.Done():
			err = bound.Err()
		}
	}
	if ended := itemEnded(request); ended != nil {
		return ended
	}
	if bound.Err() != nil {
		// The request is alive, so its writer episode ended.
		return replication.ErrFenced
	}
	return err
}

// checkLaterItem is the source admission of an item that follows a completed
// item of the same request. The request's original lease must still be held,
// and the Store's authority must be the lease's own authority advanced only in
// its installed revision. That the revision was advanced by this request's
// commits and the node's own follow-up writes, and by no command of another
// caller, is the coordinator's exact compare under the publication gate.
//
// The lease's dependency installations are deliberately not compared again.
// The node replaces them on its own schedule, for example whenever the
// provider resource owner installs a new resource set, and a request of many
// items would be cut by the first replacement. Nothing of a later item is
// decided from the retired image: the item's transaction pins the
// installations that are current when it runs and reads the raw credential,
// grants, ownership, preconditions and limits again.
func (s *servingRuntime) checkLaterItem(ctx context.Context, target serving.TermActivation) error {
	return servinghttp.CheckWriterAdvanced(ctx, s.publisher, target)
}

// itemEnded answers an item that the request's context no longer allows. The
// ordinary credential check would report an expired credential for an item
// that only ran out of the request's lifetime.
func itemEnded(ctx context.Context) error {
	if requestItemsFrom(ctx) == nil || ctx.Err() == nil {
		return nil
	}
	message := "the request's lifetime ended before this item was reached; it was not applied"
	if errors.Is(ctx.Err(), context.Canceled) {
		message = "the request was canceled before this item was reached; it was not applied"
	}
	return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: message}}
}
