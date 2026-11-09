package apiclient

import (
	"context"
	"net/http"
)

// ActivateCluster sends one reviewed activation through generated dispatch.
// Redirects, replayable bodies and idempotency headers are disabled. An unknown
// response must be inspected by the operator rather than automatically retried.
func (c *Client) ActivateCluster(ctx context.Context, in, out any) error {
	return c.do(ctx, http.MethodPost, "/cluster/activate", nil, in, out, true)
}
