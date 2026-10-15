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
