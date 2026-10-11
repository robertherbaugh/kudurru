package kube

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// contact records the last successful round trip with the apiserver, the
// store.Info.LastContact signal: zonecache stops refreshing the local.db
// snapshot timestamp once it goes stale during an apiserver outage.
//
// Every request of the driver (client, informers, discovery, Leases) goes
// through one transport wrapped by contactTracker. A response counts when
// it came from the apiserver, which is any status below 500 (a 404 or 409
// is a healthy answer; a 502/503/504 is usually a load balancer in front
// of a dead apiserver). Reads of a response body also count, so a
// long-running watch keeps the signal fresh while events stream. With the
// Lease electors renewing or polling every RetryPeriod (2 s), a healthy
// replica's LastContact is at most a few seconds old.
type contact struct {
	last atomic.Int64 // UnixNano; 0 = never
	now  func() time.Time
}

func (c *contact) mark() { c.last.Store(c.now().UnixNano()) }

// Last returns the time of the last successful round trip, or zero.
func (c *contact) Last() time.Time {
	ns := c.last.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// wrap returns rt instrumented to mark c; it is a rest.Config WrapTransport.
func (c *contact) wrap(rt http.RoundTripper) http.RoundTripper {
	return &contactTracker{next: rt, c: c}
}

type contactTracker struct {
	next http.RoundTripper
	c    *contact
}

// RoundTrip implements http.RoundTripper.
func (t *contactTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp == nil || resp.StatusCode >= http.StatusInternalServerError {
		return resp, err
	}
	t.c.mark()
	if resp.Body != nil {
		resp.Body = &contactBody{ReadCloser: resp.Body, c: t.c}
	}
	return resp, nil
}

// contactBody marks contact on every successful read of a response body.
type contactBody struct {
	io.ReadCloser
	c *contact
}

func (b *contactBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.c.mark()
	}
	return n, err
}
