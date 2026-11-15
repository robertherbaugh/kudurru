// Package vip abstracts virtual-IP ownership so the same UI, metrics and
// drain commands work with keepalived (track_file + FIFO), kube-vip and
// MetalLB (Service status) or no VIP at all (dev-docs/design-record.md §3.6).
package vip

import (
	"context"
	"time"
)

// Weight is what kudurru writes to keepalived's track_file. Because every
// instance runs nopreempt, only Fault moves a VIP away from the current
// MASTER; PreferNot only affects the next election.
type Weight int

// Weights.
const (
	Serving   Weight = 0
	PreferNot Weight = 60
	Fault     Weight = 300
)

// State is a VRRP state.
type State string

// VRRP states.
const (
	Master State = "MASTER"
	Backup State = "BACKUP"
	FaultS State = "FAULT"
	Stop   State = "STOP"
)

// VIP is one managed address.
type VIP struct {
	Name    string
	Address string
	VRID    int
	Owner   string
	Weight  Weight
}

// Event is a state transition.
type Event struct {
	VIP      string
	Node     string
	VRID     int
	Previous State
	State    State
	Priority int
	At       time.Time
}

// Provider is implemented by keepalived, kube-vip, metallb and none.
type Provider interface {
	Name() string
	VIPs() []VIP
	Owner(vip string) (node string, ok bool)
	// SetWeight is a no-op for Service-based providers.
	SetWeight(ctx context.Context, vip string, w Weight) error
	Events() <-chan Event
}

// None is the provider when no VIP is configured.
type None struct{}

// Name implements Provider.
func (None) Name() string { return "none" }

// VIPs implements Provider.
func (None) VIPs() []VIP { return nil }

// Owner implements Provider.
func (None) Owner(string) (string, bool) { return "", false }

// SetWeight implements Provider.
func (None) SetWeight(context.Context, string, Weight) error { return nil }

// Events implements Provider.
func (None) Events() <-chan Event { return nil }
