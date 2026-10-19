package follower

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Response is already transport-authenticated and byte-bounded. Term must be
// present even for 304/incompatible images so higher authority can fence.
type Response struct {
	Term        replication.Term
	Image       *replication.Image
	NotModified bool
	ETag        string
}

// Source fetches authenticated, bounded images from the current primary.
type Source interface {
	Fetch(context.Context, string) (Response, error)
}

// Status records the last successful contact, installed generation and latest error.
type Status struct {
	LastContact time.Time
	Installed   replication.Generation
	Error       error
}

// Puller serializes replication fetches and validates images before installation.
type Puller struct {
	Mirror   *sqlite.Store
	Source   Source
	Interval time.Duration
	Timeout  time.Duration
	Now      func() time.Time
	mu       sync.Mutex
	status   Status
	pullMu   sync.Mutex
}

// Status returns a detached observation of the latest pull result.
func (p *Puller) Status() Status { p.mu.Lock(); defer p.mu.Unlock(); return p.status }
func (p *Puller) remember(g replication.Generation, err error, contact bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.Error = err
	if contact {
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		p.status.LastContact = now()
		p.status.Installed = g
	}
}

// Pull fetches and installs one image after checking authority and image identity.
func (p *Puller) Pull(ctx context.Context) (err error) {
	p.pullMu.Lock()
	defer p.pullMu.Unlock()
	defer func() {
		if err != nil {
			p.remember(replication.Generation{}, err, false)
		}
	}()
	if p.Mirror == nil || p.Source == nil {
		return replication.ErrInvalid
	}
	a := p.Mirror.Authority()
	if a.Writable() {
		return replication.ErrReadOnly
	}
	current := a.Installed
	etag := ""
	if current.Term.ClusterID != "" {
		i, e := p.Mirror.ReadGeneration(ctx)
		if e != nil {
			return e
		}
		current = i.Generation()
		etag = current.ETag()
	}
	resp, err := p.Source.Fetch(ctx, etag)
	if err != nil {
		return err
	}
	if _, err = p.Mirror.ObserveAuthority(ctx, resp.Term); err != nil {
		return err
	}
	if resp.NotModified {
		if resp.Image != nil || etag == "" || resp.ETag != etag || !resp.Term.Same(current.Term) {
			return replication.ErrInvalid
		}
		p.remember(current, nil, true)
		return nil
	}
	if resp.Image == nil || !resp.Image.Term.Same(resp.Term) || (resp.ETag != "" && resp.ETag != resp.Image.Generation().ETag()) {
		return replication.ErrInvalid
	}
	v, err := p.Mirror.ValidateImage(ctx, *resp.Image)
	if err != nil {
		return err
	}
	g, err := p.Mirror.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: resp.Term, ExpectedInstalled: current})
	if err != nil {
		return err
	}
	p.remember(g, nil, true)
	return nil
}

// Run is an always-on pull loop; bad data/unreachable primary cannot stop DNS.
// The embedding coordinator supplies membership and liveness/promotion policy.
func (p *Puller) Run(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	delay := interval
	for {
		if ctx.Err() != nil {
			return nil
		}
		attempt, cancel := context.WithTimeout(ctx, timeout)
		err := p.Pull(attempt)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			delay = min(max(delay*2, interval), time.Minute)
		} else {
			delay = interval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
