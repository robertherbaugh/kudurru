package integrationconformance

import "context"

// Local filesystem calls are not universally interruptible. Bound the caller's
// wait and retain at most one pending capture per process, including a capture
// whose OS call has not returned after cancellation. It performs no network I/O
// and cannot publish late inputs. A stuck filesystem makes future captures
// unavailable until it returns; cancellation never creates another worker.
var captureSlot = make(chan struct{}, 1)

func capture(ctx context.Context, acquire func() (*runner, error)) (*runner, error) {
	select {
	case captureSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-captureSlot
		return nil, err
	}
	type result struct {
		r   *runner
		err error
	}
	done := make(chan result)
	go func() {
		defer func() { <-captureSlot }()
		r, err := acquire()
		select {
		case done <- result{r, err}:
		case <-ctx.Done():
			if r != nil && r.client != nil {
				r.client.CloseIdleConnections()
			}
		}
	}()
	select {
	case got := <-done:
		if err := ctx.Err(); err != nil {
			if got.r != nil && got.r.client != nil {
				got.r.client.CloseIdleConnections()
			}
			return nil, err
		}
		return got.r, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
