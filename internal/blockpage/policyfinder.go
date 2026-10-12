package blockpage

import (
	"context"
	"net/netip"
	"time"

	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

// IndexSource supplies the current policy Index; policy.Source implements
// it.
type IndexSource interface {
	Index() *policy.Index
}

// PolicyFinder evaluates the name against the policy that applies to the
// client address now, as the blocking stage would for its question. It
// keeps nothing, so it works under every query-log privacy mode and with
// the query log off, but it cannot see response-side (CNAME) blocks and
// identifies the client by its address only (no client ID, no MAC).
type PolicyFinder struct {
	Source IndexSource
}

// Find implements Finder.
func (f PolicyFinder) Find(_ context.Context, client netip.Addr, domain string, now time.Time) (Reason, bool, string) {
	d := f.decision(client, now)
	if !d.Filters() || d.Selection == nil {
		return Reason{}, false, ""
	}
	bd := d.Selection.Evaluate(domain + ".")
	if bd.Action != engine.ActionBlock {
		return Reason{}, false, ""
	}
	var ede uint16
	if bd.Reason == engine.ReasonAllowOnly {
		ede = 17
	}
	r := reasonOf(bd.Status(), ede, ede != 0, bd.List, bd.Rule())
	r.Policy = d.Policy
	return r, true, ""
}

// Policy returns the policy that applies to client now, "" for none.
func (f PolicyFinder) Policy(client netip.Addr, now time.Time) string {
	if d := f.decision(client, now); d.Applies() {
		return d.Policy
	}
	return ""
}

// Membership returns the client's groups and the policies that apply to
// it now (highest priority first), as an unblock request records them.
func (f PolicyFinder) Membership(client netip.Addr, now time.Time) (groups, policies []string) {
	if f.Source == nil || !client.IsValid() {
		return nil, nil
	}
	ix := f.Source.Index()
	if ix == nil {
		return nil, nil
	}
	m := ix.Identify(pipeline.Client{Addr: netip.AddrPortFrom(client.Unmap(), 0)})
	return m.GroupIDs, ix.Select(m, now).Policies
}

func (f PolicyFinder) decision(client netip.Addr, now time.Time) *policy.Decision {
	if f.Source == nil || !client.IsValid() {
		return nil
	}
	ix := f.Source.Index()
	if ix == nil {
		return nil
	}
	return ix.SelectPolicy(pipeline.Client{Addr: netip.AddrPortFrom(client.Unmap(), 0)}, now)
}

// Chain tries each Finder in turn; the first reason wins. Without one, the
// first non-empty note is returned.
type Chain []Finder

// Find implements Finder.
func (c Chain) Find(ctx context.Context, client netip.Addr, domain string, now time.Time) (Reason, bool, string) {
	note := ""
	for _, f := range c {
		if f == nil {
			continue
		}
		r, ok, n := f.Find(ctx, client, domain, now)
		if ok {
			return r, true, ""
		}
		if note == "" {
			note = n
		}
	}
	if note == "" {
		note = noteNoFinding
	}
	return Reason{}, false, note
}
