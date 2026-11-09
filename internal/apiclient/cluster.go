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

// IssueClusterJoinToken sends one reviewed invitation request without redirects,
// replayable bodies, automatic source refresh or retries after ambiguous output.
func (c *Client) IssueClusterJoinToken(ctx context.Context, in, out any) error {
	return c.do(ctx, http.MethodPost, "/cluster/join-token", nil, in, out, true)
}

// PromoteCluster sends one reviewed decision without redirects or automatic replay.
func (c *Client) PromoteCluster(ctx context.Context, in, out any) error {
	return c.do(ctx, http.MethodPost, "/cluster/promote", nil, in, out, true)
}
