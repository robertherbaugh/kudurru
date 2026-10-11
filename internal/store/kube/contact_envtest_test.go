package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// outage fails every new request and every read of an open response
// body while cut is set: an apiserver outage as the driver sees it.
type outage struct {
	cut  *atomic.Bool
	next http.RoundTripper
}

var errOutage = errors.New("partitioned")

func (p *outage) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.cut.Load() {
		return nil, errOutage
	}
	resp, err := p.next.RoundTrip(r)
	if resp != nil && resp.Body != nil {
		resp.Body = &outageBody{ReadCloser: resp.Body, p: p}
	}
	return resp, err
}

type outageBody struct {
	io.ReadCloser
	p *outage
}

func (b *outageBody) Read(buf []byte) (int, error) {
	if b.p.cut.Load() {
		return 0, errOutage
	}
	return b.ReadCloser.Read(buf)
}

// TestLastContactDuringPartition: Info().LastContact advances while the
// apiserver answers, freezes during an outage under a running driver (so
// zonecache stops refreshing the local.db timestamp) and advances again
// when the apiserver is back.
func TestLastContactDuringPartition(t *testing.T) {
	requireEnv(t)
	cut := &atomic.Bool{}
	cfg := rest.CopyConfig(shared.cfg)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &outage{cut: cut, next: rt} })
	s := newStoreOn(t, cfg, Options{})
	ctx := t.Context()
	if !s.WaitForSync(ctx) {
		t.Fatal("informers did not sync")
	}
	// The elector polls the Lease every RetryPeriod, as in serve.
	ectx, stopElector := context.WithCancel(ctx)
	defer stopElector()
	go func() { _ = s.Elector("contact-test").Run(ectx, nil, nil) }()
	first := s.Info().LastContact
	if first.IsZero() {
		t.Fatal("no contact after syncing")
	}
	eventually(t, 10*time.Second, "LastContact to advance", func() bool { return s.Info().LastContact.After(first) })

	cut.Store(true)
	cutAt := time.Now()
	time.Sleep(3 * time.Second)
	if lc := s.Info().LastContact; lc.After(cutAt) {
		t.Fatalf("LastContact %s advanced during the partition (cut at %s)", lc, cutAt)
	}

	cut.Store(false)
	healed := time.Now()
	eventually(t, 30*time.Second, "LastContact after the partition heals", func() bool { return s.Info().LastContact.After(healed) })
	t.Logf("contact frozen for the 3 s partition, back %s after healing", time.Since(healed).Round(time.Millisecond))
}
