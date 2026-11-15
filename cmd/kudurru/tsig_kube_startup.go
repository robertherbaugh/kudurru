package main

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

var errKubeTSIGUnavailable = errors.New("stored TSIG startup source unavailable; restart required")

type kubeTSIGReader interface {
	tsigkeys.Reader
	StartupClusterID(context.Context) (string, error)
}

// kubeTSIGStartup prepares exactly one complete source independently of ordinary
// saved-state startup. A nil result is pending, a result with nil prepared is
// terminally unavailable, and only Prepared certifies even an empty inventory.
// It never supplies current Kubernetes mutation or hot-rotation authority.
type kubeTSIGStartup struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	result   atomic.Pointer[kubeTSIGResult]
	unsigned *dnsserver.StaticKeyring
}

type kubeTSIGResult struct{ prepared *tsigkeys.Prepared }

func newKubeTSIGStartup(ctx context.Context, reader kubeTSIGReader, sealer *tsigkeys.Sealer, baseline *dnsserver.StaticKeyring, timeout time.Duration) (*kubeTSIGStartup, error) {
	if ctx == nil || reader == nil || sealer == nil || baseline == nil || timeout <= 0 {
		return nil, errKubeTSIGUnavailable
	}
	empty, err := dnsserver.NewStaticKeyring()
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	r := &kubeTSIGStartup{ctx: lifetime, cancel: cancel, done: make(chan struct{}), unsigned: empty}
	// Start the finite work lifetime before dispatch, never on first DNS use.
	work, finish := context.WithTimeout(lifetime, timeout)
	go func() {
		defer close(r.done)
		defer finish()
		prepared := prepareKubeTSIG(work, reader, sealer, baseline)
		if work.Err() != nil {
			prepared = nil
		}
		r.result.Store(&kubeTSIGResult{prepared: prepared})
	}()
	return r, nil
}

func prepareKubeTSIG(ctx context.Context, reader kubeTSIGReader, sealer *tsigkeys.Sealer, baseline *dnsserver.StaticKeyring) *tsigkeys.Prepared {
	cluster, err := reader.StartupClusterID(ctx)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	source, err := tsigkeys.Read(ctx, reader, cluster)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	prepared, err := source.Prepare(sealer, baseline)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	return prepared
}

func (r *kubeTSIGStartup) Close() {
	r.cancel()
	<-r.done
}

func (r *kubeTSIGStartup) keys() *dnsserver.StaticKeyring {
	if r == nil || r.ctx.Err() != nil {
		return nil
	}
	result := r.result.Load()
	if result == nil || result.prepared == nil {
		return nil
	}
	return result.prepared.Keys()
}

func (r *kubeTSIGStartup) requestKeys(req *pipeline.Request) (*dnsserver.StaticKeyring, error) {
	if r == nil || req == nil || req.Ctx == nil || req.Msg == nil || req.Ctx.Err() != nil || r.ctx.Err() != nil {
		return nil, errKubeTSIGUnavailable
	}
	keys := r.keys() // Exactly one original selection before verification.
	if keys != nil {
		return keys, nil
	}
	if req.Msg.IsTsig() != nil {
		return nil, errKubeTSIGUnavailable
	}
	// An unsigned request needs no credential. This does not publish the empty
	// ring as a complete stored source or permit any signed background job.
	return r.unsigned, nil
}

func (r *kubeTSIGStartup) captureAcquisition(dz *v1alpha1.DNSZone) dnsserver.AcquisitionKeys {
	if r == nil || r.ctx.Err() != nil || dz == nil || dz.Spec.Secondary == nil {
		return nil
	}
	if dz.Spec.Secondary.TSIGKeyRef == "" {
		return &kubeTSIGAcquisition{source: r, keys: r.unsigned, identity: "kube/unsigned"}
	}
	result := r.result.Load()
	if result == nil || result.prepared == nil {
		return nil
	}
	prepared := result.prepared
	return &kubeTSIGAcquisition{source: r, keys: prepared.Keys(), identity: prepared.SourceIdentity() + "/" + prepared.EncryptionIdentity()}
}

func (r *kubeTSIGStartup) changes() <-chan struct{} {
	if r.result.Load() != nil {
		return nil
	}
	return r.done
}

// Wait only the notifier, not DNS startup, so its initial serial round uses the
// terminal original result. There is no missed serial or later credential retry.
func (r *kubeTSIGStartup) runNotifier(ctx context.Context, run func(context.Context) error) error {
	select {
	case <-ctx.Done():
		return nil
	case <-r.done:
	}
	if ctx.Err() != nil {
		return nil
	}
	return run(ctx)
}

type kubeTSIGAcquisition struct {
	source   *kubeTSIGStartup
	keys     *dnsserver.StaticKeyring
	identity string
}

func (c *kubeTSIGAcquisition) Keys() *dnsserver.StaticKeyring { return c.keys }
func (c *kubeTSIGAcquisition) Identity() string               { return c.identity }
func (c *kubeTSIGAcquisition) Current() bool {
	return c != nil && c.source != nil && c.source.ctx.Err() == nil
}
func (c *kubeTSIGAcquisition) WithCurrent(ctx context.Context, cache func() error, publish func() error) error {
	if ctx == nil || cache == nil || !c.Current() {
		return errKubeTSIGUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cache(); err != nil {
		return err
	}
	if !c.Current() {
		return errKubeTSIGUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if publish != nil {
		return publish()
	}
	return nil
}
