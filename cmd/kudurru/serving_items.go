package main

import (
	"context"
	"errors"
	"net/http"
	"sync"

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

	mu     sync.Mutex
	bound  bool
	writer *primaryWriterTerm
	target serving.TermActivation
}

type requestItemsKey struct{}

// ObjectItems begins the item sequence of a multi-item public request. It
// captures nothing and admits nothing: every item still runs ObjectCommand
// with the request's original context values.
func (s *servingRuntime) ObjectItems(ctx context.Context) context.Context {
	if s == nil || ctx == nil || requestItemsFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, requestItemsKey{}, &requestItems{chain: s.providers.NewChain()})
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
		i.bound, i.writer, i.target = true, writer, target
		return nil
	}
	if i.writer != writer || !i.target.Term.Same(target.Term) || i.target.Incarnation != target.Incarnation {
		return replication.ErrFenced
	}
	return nil
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
