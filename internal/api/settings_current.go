package api

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// CanonicalSettingsPlanner prepares settings inside an existing current command.
// Implementations must retain final entitlement checks in that same transaction;
// they must never invoke a SettingsWriter or open a second transaction.
type CanonicalSettingsPlanner interface {
	PrepareSettings(context.Context, *sqlite.CommandTx, map[string]any) (*CanonicalSettingsPlan, error)
}

// CanonicalSettingsPlan owns a validated, detached singleton update bound to one
// concrete transaction. Admission and final proof checks remain the executor's
// responsibility, including when the settings already equal the requested values.
type CanonicalSettingsPlan struct {
	tx       *sqlite.CommandTx
	settings *public.Kudurru
	next     *config.Config
	paths    []string
	restart  []string
	changed  bool
	applied  bool
}

// Branding reports whether this operation requires Enterprise branding authority.
func (p *CanonicalSettingsPlan) Branding() bool {
	return slices.ContainsFunc(p.paths, func(path string) bool { return strings.HasPrefix(path, "branding.") })
}

// Apply stages the validated singleton once, only in its original transaction.
// Mutation errors terminate the enclosing command; callers must not journal them
// as recoverable validation failures after the transaction has been poisoned.
func (p *CanonicalSettingsPlan) Apply(ctx context.Context, tx *sqlite.CommandTx) error {
	if p == nil || tx == nil || p.tx != tx || p.applied {
		return errUnavailable("current settings plan unavailable")
	}
	p.applied = true
	if !p.changed {
		return nil
	}
	_, err := tx.Apply(ctx, p.settings, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: p.settings.ResourceVersion})
	return err
}

// PrepareCanonicalSettingsPatch merges a reviewed patch with the current
// transaction's singleton using exactly the ordinary PATCH validation rules.
func PrepareCanonicalSettingsPatch(ctx context.Context, tx *sqlite.CommandTx, host *config.Config, patch map[string]any) (*CanonicalSettingsPlan, error) {
	settings, document, cur, err := currentSettings(ctx, tx, host)
	if err != nil {
		return nil, err
	}
	if len(patch) == 0 {
		return nil, errInvalid("body: an object with the keys to change is required")
	}
	// nosemgrep: iterate-over-empty-map -- patchPaths fills changes
	changes := map[string]any{}
	if err := patchPaths("", patch, changes); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, errInvalid("settings change is empty or invalid")
	}
	next, restart, err := prepareSettingsValues(cur, config.ScopeCluster, changes, paths, true)
	if err != nil {
		return nil, err
	}
	return planCanonicalSettings(tx, settings, document, host, next, paths, restart)
}

// PrepareCanonicalSettingsValues prepares already-merged ordinary PATCH values.
// Map leaves are replacements here, preserving deletions from the earlier merge;
// imports use PrepareCanonicalSettingsPatch to merge inside their own command.
func PrepareCanonicalSettingsValues(ctx context.Context, tx *sqlite.CommandTx, host, next *config.Config, paths []string) (*CanonicalSettingsPlan, error) {
	settings, document, cur, err := currentSettings(ctx, tx, host)
	if err != nil {
		return nil, err
	}
	if next == nil || len(paths) == 0 || len(paths) > len(config.Schema) {
		return nil, errInvalid("settings change is empty or invalid")
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if _, ok := config.Lookup(path); !ok || seen[path] {
			return nil, errInvalid("settings command requires unique public cluster keys")
		}
		seen[path] = true
	}
	// These are resolved replacement values, never a second merge patch.
	// Validate paths here and the complete resulting document below.
	restart, err := validateSettingsPaths(cur, config.ScopeCluster, paths, true)
	if err != nil {
		return nil, err
	}
	return planCanonicalSettings(tx, settings, document, host, next, paths, restart)
}

func currentSettings(ctx context.Context, tx *sqlite.CommandTx, host *config.Config) (*public.Kudurru, map[string]any, *config.Config, error) {
	if tx == nil || host == nil {
		return nil, nil, nil, errUnavailable("current settings command unavailable")
	}
	settings := &public.Kudurru{}
	if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, settings); err != nil {
		return nil, nil, nil, err
	}
	document := map[string]any{}
	for key, raw := range settings.Spec.Settings {
		var value any
		if err := json.Unmarshal(raw.Raw, &value); err != nil {
			return nil, nil, nil, err
		}
		document[key] = value
	}
	cur, _, err := config.ApplyClusterDocument(host, document)
	return settings, document, cur, err
}

func planCanonicalSettings(tx *sqlite.CommandTx, settings *public.Kudurru, document map[string]any, host, next *config.Config, paths, restart []string) (*CanonicalSettingsPlan, error) {
	before, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	flat := flatSettings(next)
	for _, path := range paths {
		setPath(document, path, flat[path])
	}
	actual, _, err := config.ApplyClusterDocument(host, document)
	if err != nil {
		return nil, settingsInvalid(err)
	}
	after, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	settings.Spec.Settings = map[string]apiextensionsv1.JSON{}
	for key, value := range document {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		settings.Spec.Settings[key] = apiextensionsv1.JSON{Raw: raw}
	}
	return &CanonicalSettingsPlan{tx: tx, settings: settings, next: actual, paths: slices.Clone(paths), restart: slices.Clone(restart), changed: !bytes.Equal(before, after)}, nil
}
