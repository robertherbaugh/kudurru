package dnsdist

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/nodehealth"
)

var errRestartRequired = errors.New("dnsdist: configuration requires a validated restart")
var errValidatorRestart = errors.New("dnsdist: new node validator requires a staged restart")

// newBackend uses either the original anonymous check or a positively qualified
// node-targeted check. Generic named validators use staged configuration.
func newBackend(t *edge.Topology, b edge.Backend) string {
	hc := t.HealthCheck
	private := "kudurru-pending-" + b.ID
	return fmt.Sprintf(`local s=newServer({address=%s,id=%s,name=%s,pool={%s},useProxyProtocol=%t,weight=%d,order=%d,sockets=%d,healthCheckMode="down",checkName=%s,checkType=%s,checkInterval=%d,checkTimeout=%d,maxCheckFailures=%d,rise=%d,mustResolve=%t}); s:setDown(); s:setAuto(false)`,
		luaString(b.Address), luaString(b.ID), luaString(b.DisplayName()), luaString(private), b.ProxyProtocol, b.Weight, b.Order, b.Sockets, luaString(hc.QName), luaString(hc.QType), hc.Interval, hc.Timeout, hc.MaxFailures, hc.Rise, hc.MustResolve)
}

// membershipDiff is bounded before any console mutations. A same-UUID
// replacement uses the restart baseline rather than collide with a live ID.
func membershipDiff(old, next *edge.Topology) (added, removed []edge.Backend, err error) {
	a, b := *old, *next
	a.Backends, b.Backends = nil, nil
	if !reflect.DeepEqual(a, b) {
		return nil, nil, errRestartRequired
	}
	previous := map[string]edge.Backend{}
	wanted := map[string]bool{}
	for _, be := range old.Backends {
		previous[be.ID] = be
	}
	for _, be := range next.Backends {
		wanted[be.ID] = true
		before, ok := previous[be.ID]
		if !ok {
			added = append(added, be)
			continue
		}
		before.InternalAddress, be.InternalAddress = "", ""
		if !reflect.DeepEqual(before, be) {
			return nil, nil, errRestartRequired
		}
	}
	for _, be := range old.Backends {
		if !wanted[be.ID] {
			removed = append(removed, be)
		}
	}
	if len(added)+len(removed) > 128 {
		return nil, nil, errRestartRequired
	}
	return added, removed, nil
}

// applyMembership reports when every new member was health-qualified. On an
// uncertain result the caller restarts from old or desired validated files;
// it never assumes an unacknowledged command did not execute.
func (s *Supervisor) applyMembership(ctx context.Context, old, next *edge.Topology, publish func() error) (qualified bool, err error) {
	added, removed, err := membershipDiff(old, next)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.UpdateTimeout)
	defer cancel()
	// A runtime constructor cannot install a response validator. Current
	// first-party nodes enforce the expected owner in the versioned question;
	// proven generic-only nodes retain the original staged-validator fallback.
	checks := map[string]string{}
	legacy := false
	for _, be := range added {
		if validatorName(next, be) == "" {
			continue
		}
		classify := s.classifyBackend
		if classify == nil {
			classify = s.classifyHealth
		}
		capability, err := classify(ctx, be)
		if err != nil {
			return false, err
		}
		if capability == healthTargetV1 {
			checks[be.ID] = nodehealth.Target(be.Name)
		} else {
			legacy = true
		}
	}
	if legacy {
		s.mu.Lock()
		s.membershipMode = "legacy-validator-restart"
		s.mu.Unlock()
		return false, errValidatorRestart
	}
	s.mu.Lock()
	s.membershipMode = "console"
	if len(checks) > 0 {
		s.membershipMode = "node-target-v1"
	}
	s.mu.Unlock()

	for _, be := range added {
		if err := s.console(ctx, backendWithCheck(next, be, checks[be.ID])); err != nil {
			return false, err
		}
	}
	return s.finishMembership(ctx, added, removed, publish)
}

func (s *Supervisor) finishMembership(ctx context.Context, added, removed []edge.Backend, publish func() error) (bool, error) {
	healthCtx, healthCancel := context.WithTimeout(ctx, s.cfg.HealthTimeout)
	defer healthCancel()
	pending := slices.Clone(added)
	for len(pending) > 0 {
		keep := pending[:0]
		for _, be := range pending {
			// Returning early without the unique final marker means still down.
			script := `if not getServer(` + luaString(be.ID) + `):isUp() then return "waiting" end`
			if err := s.console(healthCtx, script); err != nil {
				keep = append(keep, be)
			}
		}
		pending = keep
		if len(pending) == 0 {
			break
		}
		select {
		case <-healthCtx.Done():
			return false, healthCtx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := publish(); err != nil {
		return false, err
	}
	for _, be := range added {
		var script strings.Builder
		script.WriteString(`local s=getServer(` + luaString(be.ID) + `); `)
		for _, pool := range be.Pools {
			script.WriteString(`s:addPool(` + luaString(pool) + `); `)
		}
		script.WriteString(`s:rmPool(` + luaString("kudurru-pending-"+be.ID) + `)`)
		if err := s.console(ctx, script.String()); err != nil {
			return true, err
		}
	}
	for _, be := range removed {
		if err := s.console(ctx, `getServer(`+luaString(be.ID)+`):setDown()`); err != nil {
			return true, err
		}
	}
	if len(removed) > 0 {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(s.cfg.PurgeDelay):
		}
	}
	for _, be := range removed {
		if err := s.console(ctx, `rmServer(`+luaString(be.ID)+`)`); err != nil {
			return true, err
		}
	}
	return true, nil
}

// stageNamedMembership retains every accepted backend in its original pools.
// New named members start forced down in private pools, with their original
// validator installed by validated configuration before the restarted child
// handles traffic. Runtime activation never bypasses automatic health.
func (s *Supervisor) stageNamedMembership(ctx context.Context, old, next *edge.Topology, oldEps []cluster.Endpoint, oldZones []string, publish func() error) (bool, error) {
	added, removed, err := membershipDiff(old, next)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.UpdateTimeout)
	defer cancel()
	staged := *old
	staged.Backends = slices.Clone(old.Backends)
	staged.Pools = slices.Clone(old.Pools)
	stagedIDs := map[string]bool{}
	for _, be := range added {
		stagedIDs[be.ID] = true
		be.Pools = []string{"kudurru-pending-" + be.ID}
		staged.Pools = append(staged.Pools, edge.Pool{Name: be.Pools[0], Policy: "leastOutstanding"})
		staged.Backends = append(staged.Backends, be)
	}
	renderer, ok := s.cfg.Renderer.(interface {
		render(*edge.Topology, map[string]bool) ([]File, error)
	})
	if !ok {
		return false, errRestartRequired
	}
	files, err := renderer.render(&staged, stagedIDs)
	if err != nil {
		return false, err
	}

	for _, f := range files {
		if err := edge.WriteFileAtomic(filepath.Join(s.cfg.ConfigDir, candidateName(f.Name)), f.Data, 0o600); err != nil {
			return false, err
		}
	}
	if err := s.check(ctx, filepath.Join(s.cfg.ConfigDir, candidateName(s.cfg.Renderer.Main()))); err != nil {
		return false, err
	}
	s.markUncertain()
	if err := s.install(files, oldEps, oldZones); err != nil {
		return false, err
	}
	if err := s.restart(ctx, "validator_staging"); err != nil {
		return false, err
	}
	// Wait for the new process's console. A failed/uncertain activation is
	// recovered by the caller from the old validated target.
	for {
		if err := s.console(ctx, "do end"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, be := range added {
		if err := s.console(ctx, `getServer(`+luaString(be.ID)+`):setAuto(false)`); err != nil {
			return false, err
		}
	}
	return s.finishMembership(ctx, added, removed, publish)
}

func backendWithCheck(top *edge.Topology, backend edge.Backend, name string) string {
	if name == "" {
		return newBackend(top, backend)
	}
	targeted := *top
	targeted.HealthCheck.QName = name
	return newBackend(&targeted, backend)
}
