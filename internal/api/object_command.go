package api

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ObjectAuthorize verifies the original raw proof once against every current
// old/new object scope. Call it once before any mutation or unchanged result.
type ObjectAuthorize func(context.Context, []rbac.Attributes) error

// ObjectCommand retains original request, writer and dependency exclusion
// through one synchronous concrete command. Only explicitly supported public
// object keys are admitted. Nil preserves the ordinary Kubernetes write path;
// an installed capability's refusal must never fall back to a live Store.
type ObjectCommand func(context.Context, store.Key,
	func(context.Context, *sqlite.CommandTx, ObjectAuthorize) error) error

// ObjectItems begins the item sequence of a request that runs one
// ObjectCommand for each of several items (POST /apply, POST
// /{kind}:batchDelete) and returns the context for those commands. The
// request's first item is admitted as a single command is; each later item is
// admitted only while no command of another caller has changed canonical state
// since the item before it (D378). Nil keeps every item an independent command.
type ObjectItems func(context.Context) context.Context

// objectItems returns the context for the items of a multi-item request.
func (s *Server) objectItems(ctx context.Context) context.Context {
	if s.opts.ObjectCommand == nil || s.opts.ObjectItems == nil {
		return ctx
	}
	return s.opts.ObjectItems(ctx)
}
