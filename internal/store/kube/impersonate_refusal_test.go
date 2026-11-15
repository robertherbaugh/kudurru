package kube

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The impersonate-verb refusal is recognized only in the apiserver's own
// form (D385): the apiserver's 403 for the driver's missing verb, legacy or
// constrained, is a deployment fault; an RBAC refusal of a principal whose
// name contains the phrase, and an admission webhook denial that uses it,
// stay the principal's 403. The refusals are built in the apiserver's
// format (k8s.io/apiserver responsewriters.ForbiddenStatusError, which
// wraps apierrors.NewForbidden); TestImpersonationWithoutImpersonateVerb
// sees the real one from envtest.
func TestImpersonateVerbDeniedMatchesOnlyTheAPIServerForm(t *testing.T) {
	const driver = "system:serviceaccount:kudurru-system:kudurru"
	im := &Impersonation{User: "kudurru:user:dave", Groups: []string{"kudurru:group:ops"}}
	refusal := func(requester, verb, group, resource, name, reason string) error {
		msg := fmt.Sprintf("User %q cannot %s resource %q in API group %q at the cluster scope", requester, verb, resource, group)
		if reason != "" {
			msg += ": " + reason
		}
		return apierrors.NewForbidden(schema.GroupResource{Group: group, Resource: resource}, name, errors.New(msg))
	}
	odd := &Impersonation{User: `mallory" cannot impersonate resource "users`}
	webhook := apierrors.NewForbidden(schema.GroupResource{Group: "kudurru.io", Resource: "dnsrecordsets"}, "www",
		errors.New(`admission webhook "policy.example" denied the request: User "x" cannot impersonate here`))
	for _, tc := range []struct {
		name string
		im   *Impersonation
		err  error
		want bool
	}{
		{"legacy user", im, refusal(driver, "impersonate", "", "users", im.User, ""), true},
		{"legacy group", im, refusal(driver, "impersonate", "", "groups", im.Groups[0], ""), true},
		{"legacy uid", im, refusal(driver, "impersonate", "authentication.k8s.io", "uids", "1234", ""), true},
		{"constrained mode", im, refusal(driver, "impersonate:user-info", "authentication.k8s.io", "users", im.User, ""), true},
		{"constrained verb", im, refusal(driver, "impersonate-on:user-info:patch", "kudurru.io", "dnsrecordsets", "www", ""), true},
		{"principal rbac", im, refusal(im.User, "patch", "kudurru.io", "dnsrecordsets", "www", ""), false},
		{"principal named like the phrase", odd, refusal(odd.User, "patch", "kudurru.io", "dnsrecordsets", "www", ""), false},
		{"principal named like the phrase with a reason", odd, refusal(odd.User, "delete", "kudurru.io", "dnsrecordsets", "www", "User \"d\" cannot impersonate"), false},
		{"principal impersonating itself", im, refusal(im.User, "impersonate", "", "users", im.User, ""), false},
		{"admission webhook", im, webhook, false},
		{"other error", im, fmt.Errorf("connection refused"), false},
	} {
		if got := impersonateVerbDenied(tc.im, tc.err); got != tc.want {
			t.Errorf("%s: impersonateVerbDenied(%q) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
