package kube

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestIsUnavailable(t *testing.T) {
	gv := schema.GroupVersion{Group: "kudurru.io", Version: "v1alpha1"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", fmt.Errorf("discovery: %w", &url.Error{Op: "Get", URL: "http://localhost/api", Err: syscall.ECONNREFUSED}), true},
		{"deadline", context.DeadlineExceeded, true},
		{"API unavailable", apierrors.NewServiceUnavailable("outage"), true},
		{"discovery unavailable", &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{gv: syscall.ECONNREFUSED}}, true},
		{"discovery forbidden", &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{gv: apierrors.NewForbidden(schema.GroupResource{Group: gv.Group, Resource: "dnszones"}, "", errors.New("denied"))}}, false},
		{"cancelled", context.Canceled, false},
		{"invalid certificate", &url.Error{Op: "Get", URL: "https://localhost/api", Err: x509.UnknownAuthorityError{}}, false},
		{"bad sqlite schema", errors.New("sqlite: malformed database schema"), false},
		{"closed store", store.ErrClosed, false},
		{"validation", apierrors.NewBadRequest("invalid object"), false},
		{"missing CRD", apierrors.NewNotFound(schema.GroupResource{Group: gv.Group, Resource: "dnszones"}, ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnavailable(tc.err); got != tc.want {
				t.Fatalf("IsUnavailable(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
