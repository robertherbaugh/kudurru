package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// outage admits I/O in connection epochs. Disconnect refuses new I/O,
// cancels the current epoch, and drains admitted round trips and reads.
// Bodies from an old epoch stay refused after reconnect.
type outage struct {
	mu      sync.Mutex
	epoch   *outageEpoch
	active  sync.WaitGroup
	refused atomic.Uint64
}

type outageEpoch struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (partition *outage) connect(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	partition.mu.Lock()
	partition.epoch = &outageEpoch{ctx: ctx, cancel: cancel}
	partition.mu.Unlock()
}

func (partition *outage) disconnect() {
	partition.mu.Lock()
	epoch := partition.epoch
	partition.epoch = nil
	partition.mu.Unlock()
	if epoch != nil {
		epoch.cancel()
	}
	partition.active.Wait()
}

func (partition *outage) admit(original *outageEpoch) *outageEpoch {
	partition.mu.Lock()
	defer partition.mu.Unlock()
	if partition.epoch == nil || (original != nil && partition.epoch != original) {
		partition.refused.Add(1)
		return nil
	}
	partition.active.Add(1)
	return partition.epoch
}

func (partition *outage) wrap(next http.RoundTripper) http.RoundTripper {
	return &outageTransport{partition: partition, next: next}
}

type outageTransport struct {
	partition *outage
	next      http.RoundTripper
}

var errOutage = errors.New("partitioned")

func (transport *outageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	partition := transport.partition
	epoch := partition.admit(nil)
	if epoch == nil {
		return nil, errOutage
	}
	defer partition.active.Done()
	ctx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(epoch.ctx, cancel)
	release := func() { stop(); cancel() }
	resp, err := transport.next.RoundTrip(request.WithContext(ctx))
	if resp != nil && resp.Body != nil {
		resp.Body = &outageBody{ReadCloser: resp.Body, partition: partition, epoch: epoch, release: release}
	} else {
		release()
	}
	return resp, err
}

type outageBody struct {
	io.ReadCloser
	partition *outage
	epoch     *outageEpoch
	release   func()
}

func (body *outageBody) Read(buf []byte) (int, error) {
	if body.partition.admit(body.epoch) == nil {
		return 0, errOutage
	}
	defer body.partition.active.Done()
	return body.ReadCloser.Read(buf)
}

func (body *outageBody) Close() error {
	body.release()
	return body.ReadCloser.Close()
}

// TestLastContactDuringPartition: Info().LastContact advances while the
// apiserver answers, freezes during an outage under a running driver (so
// zonecache stops refreshing the local.db timestamp) and advances again
// when the apiserver is back.
func TestLastContactDuringPartition(t *testing.T) {
	requireEnv(t)
	ctx := t.Context()
	partition := &outage{}
	partition.connect(ctx)
	s := newStoreOn(t, shared.cfg, Options{}, partition.wrap)
	if !s.WaitForSync(ctx) {
		t.Fatal("informers did not sync")
	}
	// The elector polls the Lease every RetryPeriod, as in serve.
	ectx, stopElector := context.WithCancel(ctx)
	defer stopElector()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Elector("contact-test").Run(ectx, nil, nil)
	}()
	t.Cleanup(func() { <-done })
	first := s.Info().LastContact
	if first.IsZero() {
		t.Fatal("no contact after syncing")
	}
	eventually(t, 10*time.Second, "LastContact to advance", func() bool { return s.Info().LastContact.After(first) })

	partition.disconnect()
	cutContact := s.Info().LastContact
	refused := partition.refused.Load()
	time.Sleep(3 * time.Second)
	if lc := s.Info().LastContact; !lc.Equal(cutContact) {
		t.Fatalf("LastContact changed during the partition: %s to %s", cutContact, lc)
	}
	if partition.refused.Load() == refused {
		t.Fatal("partition did not refuse any live transport activity")
	}

	healed := time.Now()
	partition.connect(ctx)
	eventually(t, 30*time.Second, "LastContact after the partition heals", func() bool { return s.Info().LastContact.After(healed) })
	t.Logf("contact frozen for the 3 s partition, back %s after healing", time.Since(healed).Round(time.Millisecond))
}
