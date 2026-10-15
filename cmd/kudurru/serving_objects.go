package main

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ObjectCommand supplies the generic public object routes with one actual
// current transaction. IdentityProvider replacements need exclusive resource
// admission and durable floors; RoleBinding is not routed by this capability.
// Neither those nor private kinds may fall back to ordinary Store writes.
func (s *servingRuntime) ObjectCommand(ctx context.Context, key store.Key, fn func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error) error {
	resource := objectCommandResource(key)
	if s == nil || ctx == nil || fn == nil || resource == "" {
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "current public object command unavailable"}}
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return currentCommandAPIError(err)
	}
	err = s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var active, attempted, authorized, poisoned atomic.Bool
		active.Store(true)
		defer active.Store(false)
		authorize := func(ctx context.Context, attrs []rbac.Attributes) error {
			if !active.Load() || !attempted.CompareAndSwap(false, true) || !objectCommandAttributes(key, resource, attrs) {
				poisoned.Store(true)
				return servingauth.ErrDenied
			}
			_, err := servingauth.ReauthorizeCurrentAllWithClock(ctx, tx, deps, proof, attrs, s.opts.Now)
			if err != nil {
				poisoned.Store(true)
				return err
			}
			authorized.Store(true)
			return nil
		}
		// Enforce admission even if an erroneous callback ignores an auth error
		// or returns an unchanged response without invoking authorize at all.
		check := func(context.Context) error {
			if !authorized.Load() || poisoned.Load() {
				return servingauth.ErrDenied
			}
			return nil
		}
		if err := tx.RequireCommitCheck(check); err != nil {
			return err
		}
		if err := fn(ctx, tx, authorize); err != nil {
			return err
		}
		return check(ctx)
	})
	if err == nil {
		s.publisher.Wake()
	}
	return currentCommandAPIError(err)
}

func objectCommandResource(key store.Key) string {
	if key.Name == "" || len(key.Name) > 253 {
		return ""
	}
	if key.Kind == "DNSZone" {
		if key.Namespace == "" {
			return "zones"
		}
		return ""
	}
	if key.Namespace == "" || len(key.Namespace) > 63 {
		return ""
	}
	switch key.Kind {
	case "DNSRecordSet":
		return "records"
	case "BlockList":
		return "blocklists"
	case "ClientGroup":
		return "clientgroups"
	case "Policy":
		return "policies"
	default:
		return ""
	}
}

func objectCommandAttributes(key store.Key, resource string, attrs []rbac.Attributes) bool {
	if len(attrs) == 0 || len(attrs) > 2 {
		return false
	}
	if len(attrs) > 1 && (key.Kind != "DNSRecordSet" || attrs[0].Verb != "write") {
		return false
	}
	for _, attr := range attrs {
		if attr.Resource != resource || attr.Namespace != key.Namespace || attr.Name != key.Name || attr.Verb != attrs[0].Verb || (attr.Verb != "write" && attr.Verb != "delete") {
			return false
		}
		switch key.Kind {
		case "DNSZone":
			if attr.Scope != (rbac.Scope{Kind: rbac.ScopeCluster}) && attr.Scope != (rbac.Scope{Kind: rbac.ScopeZone, Name: key.Name}) {
				return false
			}
		case "DNSRecordSet":
			if attr.Scope != (rbac.Scope{Kind: rbac.ScopeCluster}) && (attr.Scope.Kind != rbac.ScopeZone || attr.Scope.Name == "") {
				return false
			}
		default:
			if attr.Scope != (rbac.Scope{Kind: rbac.ScopeCluster}) {
				return false
			}
		}
	}
	return true
}
