package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/robertherbaugh/kudurru/internal/objects"
)

// staticObjectMutation protects ordinary and forwarded commands equally, after
// authentication/permission admission and before body decoding or provider I/O.
// Runtime sessions and the node-local blocking/cache controls remain available.
func staticObjectMutation(op huma.Operation, p perm) bool {
	if op.Method == http.MethodGet || op.Method == http.MethodHead || op.Method == http.MethodOptions {
		return false
	}
	switch op.OperationID {
	case "changePassword", "beginTOTP", "confirmTOTP", "disableTOTP", "createToken", "deleteToken":
		return true
	}
	switch p.verb {
	case "write", "delete", "decide", "import", "sync", "act":
		return true
	}
	return false
}

func (s *Server) staticWriteMW(ctx huma.Context, next func(huma.Context)) {
	if s.staticMode() {
		s.writeHumaError(ctx, newError(http.StatusServiceUnavailable, KeyReadOnly,
			objects.ErrStaticReadOnly.Error(), "edit the distributed files on every node; provision account credentials before enabling static mode"))
		return
	}
	next(ctx)
}

func (s *Server) blockingScope() string {
	if s.staticMode() {
		return "node"
	}
	return "cluster"
}
