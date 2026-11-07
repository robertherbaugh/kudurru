package apiclient

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// RoleBindingCommand sends one dedicated reviewed mutation through generated
// dispatch. It disables redirects, replayable bodies and idempotency headers.
// Successful responses retain the existing one-MiB single-use bound, which
// accommodates a supported 64-KiB desired manifest plus server metadata.
// Non-success responses preserve the original server status and error envelope.
func (c *Client) RoleBindingCommand(ctx context.Context, method, path string, in, out any) error {
	const collection = "/iam/rolebindings"
	valid := method == http.MethodPost && path == collection
	if method == http.MethodPut || method == http.MethodDelete {
		pieces := strings.Split(strings.TrimPrefix(path, collection+"/"), "/")
		valid = strings.HasPrefix(path, collection+"/") && len(pieces) == 2 && len(validation.IsDNS1123Label(pieces[0])) == 0 && len(validation.IsDNS1123Subdomain(pieces[1])) == 0
	}
	if !valid {
		return errors.New("apiclient: invalid dedicated RoleBinding operation")
	}
	return c.do(ctx, method, path, nil, in, out, true)
}
