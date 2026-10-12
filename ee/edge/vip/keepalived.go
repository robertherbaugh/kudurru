package vip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
)

// Names of the audit action and event a VIP transition produces
// (docs/architecture.md §3.6). Consumers of Provider.Events write them.
const (
	AuditVIPChanged    = "cluster.vip.changed"
	EventMasterChanged = "vip.master_changed"
)

// Reason says why a weight is being written; SetWeight enforces D51 with
// it.
type Reason string

// Reasons.
const (
	// ReasonLameduck is `kudurru lameduck`: it never FAULTs the VIP (D51);
	// the node drains out of dnsdist's backend pools instead.
	ReasonLameduck Reason = "lameduck"
	// ReasonDrain is `kudurru vip drain` / maintenance mode: writes Fault.
	ReasonDrain Reason = "drain"
	// ReasonRebalance is `kudurru vip rebalance`: Fault, then Serving.
	ReasonRebalance Reason = "rebalance"
	// ReasonHint is a leader/voter hint: PreferNot or Serving.
	ReasonHint Reason = "hint"
)

type reasonKey struct{}

// WithReason tags ctx with the reason for a SetWeight call.
func WithReason(ctx context.Context, r Reason) context.Context {
	return context.WithValue(ctx, reasonKey{}, r)
}

// ReasonFrom returns the reason ctx carries, if any.
func ReasonFrom(ctx context.Context) (Reason, bool) {
	r, ok := ctx.Value(reasonKey{}).(Reason)
	return r, ok
}

// ErrLameduckFault is returned when lameduck tries to write Fault (D51).
var ErrLameduckFault = errors.New("vip: lameduck never moves the VIP (D51); use `kudurru vip drain` to move it")

// ErrUnknownVIP is returned for a VIP the provider does not manage.
var ErrUnknownVIP = errors.New("vip: unknown VIP")

// Keepalived is the Provider for `vip.provider: keepalived`: weights go
// to the track files in RunDir that keepalived reads, ownership comes
// from the FIFO state file `kudurru vip fifo` maintains there. It runs in
// the local kudurru, which shares RunDir (the kudurru-run volume) with
// the keepalived container.
type Keepalived struct {
	// RunDir is the shared run directory; default /run/kudurru.
	RunDir string
	// Node is this node's name (the keepalived router_id).
	Node string
	// Poll is the state file poll interval of Run; default 1 s.
	Poll time.Duration
	// Log defaults to slog.Default().
	Log *slog.Logger

	vips   []VIP
	events chan Event
	mu     sync.Mutex
	state  keepalived.StateFile
}

// NewKeepalived returns the provider for the given VIPs (Name, Address,
// VRID from the host-only vip configuration).
func NewKeepalived(runDir, node string, vips []VIP) *Keepalived {
	if runDir == "" {
		runDir = keepalived.DefaultRunDir
	}
	return &Keepalived{RunDir: runDir, Node: node, vips: slices.Clone(vips), events: make(chan Event, 64)}
}

// Name implements Provider.
func (k *Keepalived) Name() string { return "keepalived" }

func (k *Keepalived) known(name string) bool {
	for _, v := range k.vips {
		if v.Name == name {
			return true
		}
	}
	return false
}

func (k *Keepalived) readState() keepalived.StateFile {
	st, err := keepalived.ReadState(k.RunDir)
	if err != nil {
		k.logger().Debug("vip state file", "err", err)
		k.mu.Lock()
		defer k.mu.Unlock()
		return k.state
	}
	return st
}

func (k *Keepalived) logger() *slog.Logger {
	if k.Log != nil {
		return k.Log
	}
	return slog.Default()
}

// VIPs implements Provider: Owner is this node while it is MASTER, Weight
// the current track-file value.
func (k *Keepalived) VIPs() []VIP {
	st := k.readState()
	out := slices.Clone(k.vips)
	for i := range out {
		out[i].Owner = ""
		if st.VIPs[out[i].Name].State == keepalived.StateMaster {
			out[i].Owner = k.Node
		}
		if w, err := keepalived.ReadWeight(k.RunDir, out[i].Name); err == nil {
			out[i].Weight = Weight(w)
		}
	}
	return out
}

// Owner implements Provider. keepalived only knows its own state: the
// owner is reported when this node is MASTER; otherwise ok is false and
// the Nodes page asks the other nodes (scatter-gather).
func (k *Keepalived) Owner(vip string) (string, bool) {
	if !k.known(vip) {
		return "", false
	}
	if k.readState().VIPs[vip].State == keepalived.StateMaster {
		return k.Node, true
	}
	return "", false
}

// SetWeight implements Provider by writing the track file. Fault moves the
// VIP (nopreempt: the only weight that does); a context tagged
// ReasonLameduck may never write it (D51).
func (k *Keepalived) SetWeight(ctx context.Context, vip string, w Weight) error {
	if !k.known(vip) {
		return fmt.Errorf("%w %q", ErrUnknownVIP, vip)
	}
	switch w {
	case Serving, PreferNot, Fault:
	default:
		return fmt.Errorf("vip: weight %d is not Serving(0), PreferNot(60) or Fault(300)", w)
	}
	if r, _ := ReasonFrom(ctx); r == ReasonLameduck && w == Fault {
		return ErrLameduckFault
	}
	if err := keepalived.WriteWeight(k.RunDir, vip, int(w)); err != nil {
		return err
	}
	r, _ := ReasonFrom(ctx)
	k.logger().Info("vip weight written", "vip", vip, "weight", int(w), "reason", string(r))
	return nil
}

// Events implements Provider; events flow while Run is running.
func (k *Keepalived) Events() <-chan Event { return k.events }

// Run polls the FIFO state file and emits an Event per VIP state change
// until ctx ends. Events are dropped (and logged) when nobody reads them:
// Run never blocks on a slow consumer.
func (k *Keepalived) Run(ctx context.Context) error {
	poll := k.Poll
	if poll <= 0 {
		poll = time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		st, err := keepalived.ReadState(k.RunDir)
		if err == nil {
			k.mu.Lock()
			prev := k.state
			k.state = st
			k.mu.Unlock()
			for _, v := range k.vips {
				cur, old := st.VIPs[v.Name], prev.VIPs[v.Name]
				if cur.State == "" || (cur.State == old.State && prev.VIPs != nil) {
					continue
				}
				ev := Event{VIP: v.Name, Node: k.Node, State: State(cur.State), Priority: cur.Priority, At: cur.Since}
				select {
				case k.events <- ev:
				default:
					k.logger().Warn("vip event dropped: no reader", "vip", v.Name, "state", cur.State)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
