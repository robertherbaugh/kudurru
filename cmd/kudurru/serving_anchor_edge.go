package main

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

var errAnchorEdgeEvidence = errors.New("root anchor edge evidence unavailable or changed")

// These are detached, explicit host requirements. An empty fleet never means
// direct serving, and a remote response cannot add/select another peer to dial.
type anchorEdgeRequirement struct {
	node, address, deployment string
	backend                   internalapi.EdgeBypassBackend
}

// The eventual activation caller must capture this original finite attempt
// before any edge acquisition. This helper neither grants activation nor reads
// or replaces canonical authority; those remain separate local admission gates.
type anchorEdgeAttempt struct {
	ctx, life context.Context
	client    *internalapi.Client
	fleet     []anchorEdgeRequirement
	used      atomic.Bool
	result    atomic.Pointer[anchorEdgeObservation]
	adopted   atomic.Pointer[rootAnchorAdmission]
}

type anchorEdgeObservation struct {
	owner  *anchorEdgeAttempt
	states []internalapi.EdgeBypassState
}

func anchorEdgeAddress(s string) bool {
	if len(s) > 512 {
		return false
	}
	host, port, err := net.SplitHostPort(s)
	n, portErr := strconv.Atoi(port)
	return err == nil && portErr == nil && host != "" && n > 0 && n <= 65535 && !strings.ContainsAny(host, "/?#@\\% \t\r\n")
}

func captureAnchorEdgeAttempt(ctx, life context.Context, client *internalapi.Client, self string, fleet []anchorEdgeRequirement) (*anchorEdgeAttempt, error) {
	if ctx == nil || life == nil || client == nil || self == "" || len(self) > 256 || len(fleet) == 0 || len(fleet) > 32 {
		return nil, errAnchorEdgeEvidence
	}
	deadline, finite := ctx.Deadline()
	if !finite || time.Until(deadline) > 30*time.Second {
		return nil, errAnchorEdgeEvidence
	}
	// Validate the ENTIRE declared fleet before contacting any member. No
	// partial success can repair an omitted, duplicate or malformed requirement.
	nodes, addresses, deployments := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range fleet {
		if e.node == "" || len(e.node) > 256 || !anchorEdgeAddress(e.address) ||
			!internalapi.ValidDigest(e.deployment) || e.deployment == strings.Repeat("0", 64) ||
			e.backend.Name != self || !anchorEdgeAddress(e.backend.Address) || !anchorEdgeAddress(e.backend.InternalAddress) ||
			nodes[e.node] || addresses[e.address] || deployments[e.deployment] {
			return nil, errAnchorEdgeEvidence
		}
		nodes[e.node], addresses[e.address], deployments[e.deployment] = true, true, true
	}
	a := &anchorEdgeAttempt{ctx: ctx, life: life, client: client, fleet: append([]anchorEdgeRequirement(nil), fleet...)}
	if err := a.check(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *anchorEdgeAttempt) check() error {
	if a == nil || a.ctx == nil || a.life == nil {
		return errAnchorEdgeEvidence
	}
	if err := a.ctx.Err(); err != nil {
		return err
	}
	return a.life.Err()
}

func (a *anchorEdgeAttempt) observe() (*anchorEdgeObservation, error) {
	if err := a.check(); err != nil {
		return nil, err
	}
	if !a.used.CompareAndSwap(false, true) {
		return nil, errAnchorEdgeEvidence
	}
	// Runtime shutdown interrupts an in-flight body too; joining the callback
	// keeps cancellation work inside this original acquisition's lifetime.
	ctx, cancel := context.WithCancel(a.ctx)
	joined := make(chan struct{})
	stop := context.AfterFunc(a.life, func() { cancel(); close(joined) })
	defer func() {
		cancel()
		if !stop() {
			<-joined
		}
	}()
	result := &anchorEdgeObservation{owner: a, states: make([]internalapi.EdgeBypassState, 0, len(a.fleet))}
	for _, e := range a.fleet {
		if err := a.check(); err != nil {
			return nil, err
		}
		state, err := a.client.EdgeBypass(ctx, e.address, e.node, e.deployment)
		if err != nil {
			return nil, err
		}
		matches := 0
		for _, backend := range state.Backends {
			if backend == e.backend {
				matches++
			}
		}
		if matches != 1 {
			return nil, errAnchorEdgeEvidence
		}
		result.states = append(result.states, state)
	}
	if err := a.check(); err != nil {
		return nil, err
	}
	a.result.Store(result)
	if err := result.check(); err != nil {
		return nil, err
	}
	return result, nil
}

// check is memory-only for the eventual ORIGINAL local activation boundary.
// Copying equal fields, waiting beyond the original deadline, or replacing a
// failed attempt cannot promote this observation into fresh evidence. It is
// intentionally finite. Only the startup Store barrier can adopt the exact
// observation into runtime life; calling this check never extends its lifetime.
func (o *anchorEdgeObservation) check() error {
	if o == nil || o.owner == nil || o.owner.result.Load() != o || len(o.states) != len(o.owner.fleet) {
		return errAnchorEdgeEvidence
	}
	return o.owner.check()
}
