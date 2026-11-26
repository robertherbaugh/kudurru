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
	paced  time.Time     // when the node last had a current publication for this request
	wait   time.Duration // the bound of the next wait for a publication; zero is pace.wait
	began  time.Time     // when the request's first item was bound
	waited time.Duration // the request's time in pace waits and rests so far
}

// itemPace bounds how long one multi-item request may keep other callers of
// the node without a current publication (D378). Every commit makes the
// publication that every other API request is bound to stale until the node
// has published again, and the items of one request commit back to back. DNS
// does not depend on it: ordinary answers use the publication that is served
// (recursiveRootSeal.servedCurrent). Before a later item, the request first
// waits, inside its lifetime, until the node has published everything
// committed so far, and then commits nothing for rest, or until the callers
// that waited have been served, so that other callers are served from that
// publication (D421). It does so after work of its items, and
// at once when another caller's read waits for a publication
// (Publisher.AwaitingHTTP) and the request has not yet spent longer in its
// waits than in its own work, so that the read is served within its own
// capture wait whenever the node publishes within it.
//
// The wait is bounded and adapts to the node's time to publish: it begins at
// wait, doubles after every wait that ended without a publication, up to
// maxWait, and after a publication is twice the time that publication took,
// never below wait. The work between two waits is at least as long as the
// next wait's bound. A node that publishes slowly (a large data set or a
// loaded machine) is therefore still waited for, instead of being given up on
// after a fixed bound that it never meets, while the waits never take much
// longer than the request's own work: the pace makes a request at most about
// twice as slow, and the request's lifetime still bounds it. A zero maxWait
// keeps every wait at wait and every work budget at work; a zero wait waits
// without bound.
type itemPace struct {
	work    time.Duration
	rest    time.Duration
	wait    time.Duration
	maxWait time.Duration
}

// defaultItemPace keeps a long request's other callers unavailable for about
// two seconds at a time, plus the node's own time to publish, instead of for
// the whole request. A request that ends within two seconds never waits. A
// shorter work budget costs a long request far more: the node's time to
// publish grows with the data it serves (measured on 150 new record sets
// without the race detector: 3.9 s unpaced, 26 s with a half-second budget,
// and waits from 0.2 s to 1.5 s). Under the race detector on a loaded machine
// a publication of 120 record sets took 1 to 2.5 s, which the earlier fixed
// two-second bound rarely met (one publication in 66 s of one request).
var defaultItemPace = itemPace{work: 2 * time.Second, rest: 200 * time.Millisecond, wait: 2 * time.Second, maxWait: 10 * time.Second}

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
		now := time.Now()
		i.bound, i.writer, i.target, i.paced, i.began = true, writer, target, now, now
		return nil
	}
	if i.writer != writer || !i.target.Term.Same(target.Term) || i.target.Incarnation != target.Incarnation {
		return replication.ErrFenced
	}
	return nil
}

// due reports whether a later item must first let the node publish. An
// adaptive pace works at least as long as its next wait may last, so that the
// waits never take longer than the work between them. awaiting reports that
// another HTTP caller is waiting for a publication now (Publisher.AwaitHTTP):
// the item then lets the node publish for it at once, as long as the request
// has not spent more time in its waits than in its own work, so that the
// other caller is served within its own capture wait instead of after the
// next work budget.
func (i *requestItems) due(awaiting bool) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	work := i.pace.work
	if i.pace.maxWait > 0 {
		work = max(work, i.wait)
	}
	if time.Since(i.paced) >= work {
		return true
	}
	return awaiting && i.waited <= time.Since(i.began)-i.waited
}

// waitBound is the bound of the next wait for a publication.
func (i *requestItems) waitBound() time.Duration {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.wait > 0 {
		return i.wait
	}
	return i.pace.wait
}

// published records that the node had a current publication for this
// request after a wait of took, and that the request then rested on it for
// rested.
func (i *requestItems) published(took, rested time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.paced = time.Now()
	i.waited += took + max(rested, 0)
	if i.pace.maxWait > 0 {
		i.wait = min(max(i.pace.wait, 2*took), max(i.pace.wait, i.pace.maxWait))
	}
}

// missed records a wait that ended at its bound without a publication: the
// request goes on, and waits again, for longer, after its next work budget.
func (i *requestItems) missed(took time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.paced = time.Now()
	i.waited += took
	if i.pace.maxWait > 0 {
		i.wait = min(2*max(i.wait, i.pace.wait), max(i.pace.wait, i.pace.maxWait))
	}
}

// paceLaterItem lets the node publish the request's earlier items before a
// later item when the request has worked for its pace, and then keeps that
// publication current for the pace's rest. It never captures a new lease and
// admits nothing: the item is still admitted only by the chain's compare, so a
// command of another caller that commits while the request waits stops every
// later item. request is the request's own context, which carries its
// lifetime; bound additionally ends with the request's writer episode.
//
// The rest ends early once every caller that waited for a publication during
// it has been served from it (Publisher.HoldTerm), so a yield costs the
// request about the node's time to publish rather than that plus the whole
// rest. A publication that goes stale while a caller still waits does not end
// the pace: each item's commit is followed by the node's own follow-up writes
// (the record's status, the zone's serial and lifecycle head, primary
// admission), published in turn, and some react to the item's first
// publication and replace it within milliseconds, before the waiting caller
// may have been scheduled to capture it. That caller then found no current
// publication and, once the request committed on, was refused with
// generation_pending when its capture wait ended. The request instead waits
// for the next publication, within the same bound, and rests on it again.
func (s *servingRuntime) paceLaterItem(request, bound context.Context, items *requestItems, target serving.TermActivation) error {
	awaited := s.publisher.AwaitingHTTP()
	if !items.due(awaited) {
		return nil
	}
	wait, cancel := bound, context.CancelFunc(func() {})
	if limit := items.waitBound(); limit > 0 {
		wait, cancel = context.WithTimeout(bound, limit)
	}
	defer cancel()
	began := time.Now()
	var err error
	for {
		err = s.publisher.WaitTerm(wait, target)
		if err != nil && bound.Err() == nil && wait.Err() != nil {
			// The node did not publish within the wait's bound: go on without
			// the publication, and wait longer after the next work budget.
			items.missed(time.Since(began))
			return nil
		}
		if err != nil {
			break
		}
		// took is the time the node needed to reach the publication that
		// the rest begins on.
		took := time.Since(began)
		if items.pace.rest <= 0 {
			items.published(took, 0)
			return nil
		}
		rested := time.Now()
		var held bool
		held, err = s.publisher.HoldTerm(bound, target, items.pace.rest, awaited)
		if err != nil {
			break
		}
		if held || !s.publisher.AwaitingHTTP() {
			// The publication stayed current for the rest or served every
			// waiting caller, or it went stale while nobody waits for it.
			items.published(took, time.Since(rested))
			return nil
		}
		if wait.Err() != nil {
			// A caller still waits, and the node did not publish again
			// within the bound.
			items.missed(time.Since(began))
			return nil
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
