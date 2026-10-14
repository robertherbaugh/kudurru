package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// PublicClusterKey reports whether the singleton may own this setting. The
// admission schema generator uses the identical predicate.
func PublicClusterKey(k Key) bool { return k.Scope == ScopeCluster && !k.HostOnly && !k.WriteOnly }

// ClusterSeed captures the primary's initial public settings. Environment
// overrides remain local; locked values use compiled defaults in the seed.
// A stored singleton is never reseeded by a later host restart.
func ClusterSeed(cfg *Config) map[string]any {
	flat := Flatten(cfg)
	doc := map[string]any{}
	for _, k := range Schema {
		if !PublicClusterKey(k) {
			continue
		}
		value := flat[k.Path]
		if slices.Contains(cfg.EnvLocked, k.Path) {
			value = k.Default
		}
		setPath(doc, k.Path, value)
	}
	return doc
}

// ApplyClusterDocument prepares a complete effective configuration from one
// canonical settings document and the local host inputs. Removed public keys
// return to schema defaults; they cannot inherit a previous serving generation.
// Local environment locks take precedence and are listed in ignored.
func ApplyClusterDocument(host *Config, document map[string]any) (next *Config, ignored []string, err error) {
	// JSON copying rejects unsupported values and keeps caller-owned maps from
	// aliasing the configuration prepared for publication.
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, nil, fmt.Errorf("cluster settings: %w", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, nil, err
	}
	if err := clusterNames("", tree); err != nil {
		return nil, nil, err
	}
	layer := map[string]any{}
	if err := settingsLeaves("", tree, layer); err != nil {
		return nil, nil, err
	}
	flat := Flatten(host)
	for path := range layer {
		k, _ := Lookup(path)
		if !PublicClusterKey(k) {
			return nil, nil, fmt.Errorf("cluster settings: %s is host-only or secret", path)
		}
	}
	locked := map[string]any{}
	for _, k := range Schema {
		if !PublicClusterKey(k) {
			continue
		}
		if slices.Contains(host.EnvLocked, k.Path) {
			locked[k.Path] = flat[k.Path]
			if _, present := layer[k.Path]; present {
				ignored = append(ignored, k.Path)
			}
		}
		flat[k.Path] = k.Default
		if value, ok := layer[k.Path]; ok {
			flat[k.Path] = value
		}
	}
	if next, err = FromFlat(flat); err != nil {
		return nil, nil, err
	}
	// Validate canonical values even when this host overrides them; another
	// member must never receive a malformed value hidden by a local lock.
	for path, value := range locked {
		flat[path] = value
	}
	if len(locked) > 0 {
		if next, err = FromFlat(flat); err != nil {
			return nil, nil, err
		}
	}
	next.EnvLocked, next.Source, next.SettingsFile = slices.Clone(host.EnvLocked), host.Source, host.SettingsFile
	next.Node.Name = host.Node.Name
	sort.Strings(ignored)
	return next, ignored, nil
}

func clusterNames(prefix string, tree map[string]any) error {
	for key, value := range tree {
		if strings.Contains(key, ".") {
			return fmt.Errorf("cluster settings: use nested fields, not dotted key %q", key)
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if _, leaf := Lookup(path); leaf {
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			if err := clusterNames(path, nested); err != nil {
				return err
			}
		}
	}
	return nil
}
