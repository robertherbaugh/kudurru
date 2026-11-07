package kube

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
)

// IsUnavailable reports failures for which startup may wait for the API
// while DNS serves local.db. Authorization, schema, certificate and local
// persistence errors must still fail startup.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errIdentityUnavailable) {
		return true
	}
	var groups *discovery.ErrGroupDiscoveryFailed
	if errors.As(err, &groups) {
		if len(groups.Groups) == 0 {
			return false
		}
		for _, cause := range groups.Groups {
			if !IsUnavailable(cause) {
				return false
			}
		}
		return true
	}
	if apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) {
		return true
	}
	for _, cause := range []error{context.DeadlineExceeded, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETUNREACH, syscall.EHOSTUNREACH, io.EOF, io.ErrUnexpectedEOF} {
		if errors.Is(err, cause) {
			return true
		}
	}
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}
