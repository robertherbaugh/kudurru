package api

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/settings"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ForwardedSettings is one cluster-scope PATCH /settings that an enrolled
// SQLite follower hands to its original primary (D368): the route's merge patch
// and its optional If-Match. It carries no permission and no merged values: the
// primary validates and merges the patch again inside its own transaction.
type ForwardedSettings struct {
	Patch   map[string]any `json:"patch"`
	IfMatch string         `json:"ifMatch,omitempty"`
}

// SettingsForward hands one cluster settings patch to the enrolled primary.
// handled=false means this node runs its own current command; handled=true
// never falls back, and an error after handling may be an unknown outcome that
// must not be replayed. The body is the primary's own route answer.
type SettingsForward func(context.Context, ForwardedSettings) (*SettingsBody, bool, error)

// SettingsAdmit is the primary's final admission of one prepared settings plan
// inside the member transaction, such as the branding entitlement of that
// transaction's pinned license. It may register commit checks on tx.
type SettingsAdmit func(context.Context, *sqlite.CommandTx, *CanonicalSettingsPlan) error

// changedPaths flattens a merge patch into its sorted schema keys with the
// same rules as the PATCH route.
func changedPaths(patch map[string]any) (map[string]any, []string, error) {
	if len(patch) == 0 {
		return nil, nil, errInvalid("body: an object with the keys to change is required")
	}
	// nosemgrep: iterate-over-empty-map -- patchPaths fills changes
	changes := map[string]any{}
	if err := patchPaths("", patch, changes); err != nil {
		return nil, nil, err
	}
	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return changes, paths, nil
}

// canonicalIfMatch is the If-Match rule of a canonical cluster patch: the
// canonical store has no upstreams section revision, so a stated revision for
// an upstreams key never matches.
func canonicalIfMatch(paths []string, ifMatch string) error {
	if ifMatch != "" && slices.ContainsFunc(paths, func(path string) bool { return strings.HasPrefix(path, "upstreams.") }) && strings.Trim(ifMatch, "\"") != "" {
		return ownershipError(settings.ErrConflict)
	}
	return nil
}

// ForwardedSettingsCommand runs one forwarded PATCH /settings inside the
// primary's member transaction tx: the same key, scope, lock and value checks
// as the primary's own route, merged with the transaction's current
// singleton, then admit, then one staged write. It returns the route's own
// answer. An error before the write is the route's public refusal; an error
// from the write terminates the enclosing command.
func (s *Server) ForwardedSettingsCommand(ctx context.Context, tx *sqlite.CommandTx, in ForwardedSettings, admit SettingsAdmit) (*SettingsBody, error) {
	if s == nil || tx == nil || admit == nil || s.opts.CanonicalSettings == nil {
		return nil, errUnavailable("settings forwarding unavailable")
	}
	_, paths, err := changedPaths(in.Patch)
	if err != nil {
		return nil, err
	}
	if err := canonicalIfMatch(paths, in.IfMatch); err != nil {
		return nil, err
	}
	plan, err := PrepareCanonicalSettingsPatch(ctx, tx, s.opts.Config, in.Patch, s.authControlsHotReload())
	if err != nil {
		return nil, err
	}
	if err := admit(ctx, tx, plan); err != nil {
		return nil, err
	}
	if err := plan.Apply(ctx, tx); err != nil {
		return nil, &forwardedSettingsWriteError{err}
	}
	out := s.settingsBody(plan.next, config.ScopeCluster, plan.paths, plan.restart)
	persisted := true
	out.Body.Persisted = &persisted
	return out.Body, nil
}

// forwardedSettingsWriteError marks a failure of the staged write itself,
// which is never a retained refusal.
type forwardedSettingsWriteError struct{ err error }

func (e *forwardedSettingsWriteError) Error() string { return e.err.Error() }
func (e *forwardedSettingsWriteError) Unwrap() error { return e.err }

// ForwardedSettingsRefusal classifies an error of ForwardedSettingsCommand: a
// client refusal before the write returns its public error so that the
// follower relays it exactly. A failed write, an authorization failure and
// every other error stay outside the retained result.
func ForwardedSettingsRefusal(err error) (*Error, bool) {
	var write *forwardedSettingsWriteError
	if errors.As(err, &write) {
		return nil, false
	}
	return ForwardedObjectRefusal(err)
}
