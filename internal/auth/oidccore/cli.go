package oidccore

import (
	"context"
	"slices"
	"time"
)

// EvaluateCLI narrows public login to the explicitly configured CLI client.
// Global discovery of auth method "none" does not establish that a confidential
// web client is public. Missing CLIClientID therefore refuses this new flow.
// Ordinary bearer/browser policies and their evidence modes remain unchanged.
func (p *Policy) EvaluateCLI(ctx context.Context, t *VerifiedToken, now time.Time) (Identity, error) {
	if p == nil || p.spec.CLIClientID == "" || t == nil {
		return Identity{}, ErrAudience
	}
	id, e := p.Evaluate(ctx, t, BearerIDToken, now)
	if e != nil {
		return Identity{}, e
	}
	azp, exists := t.claims["azp"]
	if !slices.Contains(t.facts.Audiences, p.spec.CLIClientID) || len(t.facts.Audiences) > 1 && !exists || exists && azp != p.spec.CLIClientID {
		return Identity{}, ErrAudience
	}
	return id, nil
}
