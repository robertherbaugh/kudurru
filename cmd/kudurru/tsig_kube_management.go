package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// kubeTSIGRefreshInterval bounds how long another replica's TSIG lifecycle
// change takes to become this replica's signing and verification ring.
const kubeTSIGRefreshInterval = 10 * time.Second

// kubeTSIGLive extends the startup source with hot replacement. Each refresh
// reads one complete stored source. A changed source that prepares completely
// with the original sealer and file baseline replaces the published ring; a
// changed source that does not prepare withdraws every stored and file
// signature (as at startup) until a later source prepares. A failed read keeps
// the current ring: the source is not known to have changed.
type kubeTSIGLive struct {
	startup  *kubeTSIGStartup
	reader   kubeTSIGReader
	writer   tsigkeys.StoreWriter
	sealer   *tsigkeys.Sealer
	mu       sync.Mutex // serializes local commands and refreshes
	cluster  string
	baseline *dnsserver.StaticKeyring
	identity string
	changed  chan struct{}
	cmu      sync.Mutex
}

func newKubeTSIGLive(startup *kubeTSIGStartup, reader kubeTSIGReader, writer tsigkeys.StoreWriter, sealer *tsigkeys.Sealer) *kubeTSIGLive {
	return &kubeTSIGLive{startup: startup, reader: reader, writer: writer, sealer: sealer, changed: make(chan struct{})}
}

// bind captures the original cluster, file baseline and source identity from
// the first successful startup preparation; without it no refresh can run.
func (l *kubeTSIGLive) bind() bool {
	if l.cluster != "" {
		return true
	}
	result := l.startup.result.Load()
	if result == nil || result.prepared == nil {
		return false
	}
	l.cluster, l.baseline, l.identity = result.prepared.ClusterID(), result.prepared.Baseline(), result.prepared.SourceIdentity()
	return l.cluster != "" && l.baseline != nil
}

// changes broadcasts every ring replacement after the initial preparation.
func (l *kubeTSIGLive) changes() <-chan struct{} {
	if l.startup.result.Load() == nil {
		return l.startup.done
	}
	l.cmu.Lock()
	defer l.cmu.Unlock()
	return l.changed
}

func (l *kubeTSIGLive) broadcast() {
	l.cmu.Lock()
	close(l.changed)
	l.changed = make(chan struct{})
	l.cmu.Unlock()
}

// refresh publishes the current complete stored source. It returns the
// published preparation, nil when signing is withdrawn.
func (l *kubeTSIGLive) refresh(ctx context.Context) (*tsigkeys.Prepared, error) {
	if !l.bind() {
		return nil, errKubeTSIGUnavailable
	}
	current := l.startup.result.Load().prepared
	source, err := tsigkeys.Read(ctx, l.reader, l.cluster)
	if err != nil {
		return current, err
	}
	if source.Identity() == l.identity {
		return current, nil
	}
	prepared, err := source.Prepare(l.sealer, l.baseline)
	l.identity = source.Identity()
	if err != nil {
		prepared = nil
	}
	l.startup.result.Store(&kubeTSIGResult{prepared: prepared})
	l.broadcast()
	return prepared, err
}

func (l *kubeTSIGLive) run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-l.startup.done:
	}
	for {
		timer := time.NewTimer(kubeTSIGRefreshInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		l.mu.Lock()
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = l.refresh(attempt)
		cancel()
		l.mu.Unlock()
	}
}

// reads lists the published ring's safe metadata (file and stored keys).
func (l *kubeTSIGLive) reads(ctx context.Context) ([]api.TSIGKeyMetadata, error) {
	if ctx == nil {
		return nil, tsigKeyUnsupported()
	}
	result := l.startup.result.Load()
	if result == nil || result.prepared == nil {
		return nil, &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "stored TSIG source unavailable"}}
	}
	metadata, err := result.prepared.Metadata()
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	out := make([]api.TSIGKeyMetadata, 0, len(metadata))
	for _, m := range metadata {
		out = append(out, tsigKeyAPIMetadata(m))
	}
	return out, nil
}

// command executes one stored TSIG lifecycle command on Kubernetes. The route
// already authorized the caller. The plan is bound to the published source;
// ApplyStore requires that exact source to be current and writes with exact
// object versions. The receipt's Published reports this replica's ring.
func (l *kubeTSIGLive) command(ctx context.Context, change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
	if l == nil || ctx == nil {
		return nil, tsigKeyUnsupported()
	}
	ctx, cancel := context.WithTimeout(ctx, tsigKeyCommandBound)
	defer cancel()
	change.KeyName = kinds.CanonicalName(change.KeyName)
	if _, err := tsigKeyAttributes(change); err != nil {
		return nil, tsigKeyAPIError(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prepared, err := l.refresh(ctx)
	if prepared == nil {
		// An unpreparable source still admits removal of an unusable carrier.
		if change.Operation == api.TSIGKeyRevoke && l.bind() {
			metadata, revokeErr := tsigkeys.RevokeUnusable(ctx, l.writer, l.sealer, change.KeyName, change.UID, change.ResourceVersion)
			if revokeErr != nil {
				return nil, tsigKeyAPIError(revokeErr)
			}
			after, _ := l.refresh(ctx)
			return &api.TSIGKeyCommandReceipt{Operation: change.Operation, Committed: true, Published: after != nil && ringReflects(after, change.Operation, metadata), Removed: true, Key: tsigKeyAPIMetadata(metadata)}, nil
		}
		return nil, tsigKeyAPIError(errors.Join(tsigkeys.ErrSource, err))
	}
	plan, err := prepared.Plan(l.sealer, tsigkeys.MutationInput{Operation: tsigkeys.Operation(change.Operation), KeyName: change.KeyName, Algorithm: change.Algorithm, UID: change.UID, ResourceVersion: change.ResourceVersion, Secret: change.Secret})
	var metadata tsigkeys.Metadata
	switch {
	case err == nil:
		metadata, err = plan.ApplyStore(ctx, l.writer)
	case change.Operation == api.TSIGKeyRevoke && errors.Is(err, tsigkeys.ErrPrecondition):
		// A carrier the published ring cannot contain (an interrupted create)
		// is removable only by its exact observed UID and resourceVersion.
		metadata, err = tsigkeys.RevokeUnusable(ctx, l.writer, l.sealer, change.KeyName, change.UID, change.ResourceVersion)
	}
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	after, refreshErr := l.refresh(ctx)
	published := refreshErr == nil && after != nil && ringReflects(after, change.Operation, metadata)
	return &api.TSIGKeyCommandReceipt{Operation: change.Operation, Committed: true, Published: published, Removed: change.Operation == api.TSIGKeyRevoke, Key: tsigKeyAPIMetadata(metadata)}, nil
}

func ringReflects(p *tsigkeys.Prepared, operation api.TSIGKeyOperation, metadata tsigkeys.Metadata) bool {
	rows, err := p.Metadata()
	if err != nil {
		return false
	}
	for _, row := range rows {
		if row.KeyName == metadata.KeyName && row.Source == "store" {
			return operation != api.TSIGKeyRevoke && row.UID == metadata.UID && row.ResourceVersion == metadata.ResourceVersion
		}
	}
	return operation == api.TSIGKeyRevoke
}
