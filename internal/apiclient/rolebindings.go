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

// IAMCommand sends one reviewed IAM writer command (D396) like
// RoleBindingCommand: no redirect, no replayable body, no idempotency header,
// and a bounded single-use answer. A token issued by the command is in the
// answer only; the caller must not log it.
func (c *Client) IAMCommand(ctx context.Context, method, path string, in, out any) error {
	segments := func(prefix string) []string {
		if !strings.HasPrefix(path, prefix+"/") {
			return nil
		}
		return strings.Split(strings.TrimPrefix(path, prefix+"/"), "/")
	}
	label := func(s string) bool { return len(validation.IsDNS1123Label(s)) == 0 }
	subdomain := func(s string) bool { return len(validation.IsDNS1123Subdomain(s)) == 0 }
	valid := false
	switch method {
	case http.MethodPost:
		sa := segments("/iam/serviceaccounts")
		u := segments("/iam/users")
		valid = path == "/iam/serviceaccounts" || path == "/iam/identityproviders" || path == "/iam/users" ||
			len(sa) == 3 && label(sa[0]) && subdomain(sa[1]) && sa[2] == "tokens" ||
			len(u) == 2 && subdomain(u[0]) && u[1] == "password"
	case http.MethodPut, http.MethodDelete, http.MethodPatch:
		if sa := segments("/iam/serviceaccounts"); method != http.MethodPatch && len(sa) == 2 {
			valid = label(sa[0]) && subdomain(sa[1])
		}
		if idp := segments("/iam/identityproviders"); method != http.MethodPatch && len(idp) == 1 {
			valid = label(idp[0])
		}
		if u := segments("/iam/users"); method != http.MethodPut && len(u) == 1 {
			valid = subdomain(u[0])
		}
	}
	if !valid {
		return errors.New("apiclient: invalid dedicated IAM operation")
	}
	return c.do(ctx, method, path, nil, in, out, true)
}
