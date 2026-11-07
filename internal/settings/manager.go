package settings

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/config"
	yaml "go.yaml.in/yaml/v3"
)

// Section describes the current authority. Revision fences both durable state
// and effective configuration, including pending changes to the file source.
type Section struct {
	Owner      string `json:"owner" enum:"gitops,ui"`
	Switchable bool   `json:"switchable"`
	Revision   string `json:"revision,omitempty"`
	EnvLocked  bool   `json:"envLocked"`
}

// Status reports section owners and file conflicts.
type Status struct {
	Sections  map[string]Section `json:"sections"`
	Conflicts []string           `json:"conflicts"`
}

// Manager serializes local writes and reloads; storage revisions fence replicas.
type Manager struct {
	mu      sync.Mutex
	Backend Backend
	Reload  *config.Reloadable
	Environ []string
	Logger  *slog.Logger
	status  Status
}

func splitPath(p string) []string { return strings.Split(p, ".") }

func upstreamValue(c *config.Config) any {
	raw, _ := json.Marshal(c.Upstreams)
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	delete(v, "importFile") // host source paths cannot become UI-owned declarations
	return v
}

func (m *Manager) owner(d Document, section string) string {
	if owner := d.Owners[section]; owner != "" {
		return owner
	}
	if m.Reload.Current().Store.Driver == "kube" {
		return GitOps
	}
	// Docker's writable settings file retains its existing behavior.
	return UI
}

func (m *Manager) load(d Document) (*config.Config, error) {
	return config.LoadWithSections(m.Reload.Current().Source, m.Environ, func(*config.Config) (map[string]any, error) { return d.Sections(), nil })
}

func (m *Manager) statusFor(d Document, revision string, c *config.Config) Status {
	locked := []string{}
	for _, path := range c.EnvLocked {
		if strings.HasPrefix(path, "upstreams.") {
			locked = append(locked, path)
		}
	}
	raw, _ := json.Marshal([]any{revision, c.FileRevision, upstreamValue(c), locked})
	token := fmt.Sprintf("%x", sha256.Sum256(raw))
	s := Status{Sections: map[string]Section{}, Conflicts: []string{}}
	for _, key := range config.Schema {
		if key.Scope != config.ScopeCluster {
			continue
		}
		section, _, _ := strings.Cut(key.Path, ".")
		s.Sections[section] = Section{Owner: m.owner(d, section)}
	}
	s.Sections["upstreams"] = Section{Owner: m.owner(d, "upstreams"), Switchable: m.Backend != nil, Revision: token, EnvLocked: len(locked) > 0}
	if d.Owners["upstreams"] == UI && c.Store.Driver == "kube" && slices.Contains(c.DeclaredSections, "upstreams") {
		s.Conflicts = append(s.Conflicts, "upstreams")
	}
	return s
}

func (m *Manager) install(d Document, revision string, fresh *config.Config) ([]string, error) {
	next, err := config.MergeCluster(m.Reload.Current(), fresh)
	if err != nil {
		return nil, err
	}
	status := m.statusFor(d, revision, fresh)
	if !reflect.DeepEqual(status.Conflicts, m.status.Conflicts) && len(status.Conflicts) > 0 {
		log := m.Logger
		if log == nil {
			log = slog.Default()
		}
		log.Warn("GitOps declares a UI-owned settings section; UI values remain authoritative", "conflicts", status.Conflicts)
	}
	m.status = status
	return m.Reload.Set(next), nil
}

func (m *Manager) read(ctx context.Context) (Document, string, *config.Config, error) {
	d, revision, err := m.Backend.Read(ctx)
	if err != nil {
		return d, revision, nil, err
	}
	fresh, err := m.load(d)
	return d, revision, fresh, err
}

// Initialize permits last-good shared settings during offline startup.
func (m *Manager) Initialize(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, rev, err := ReadStartup(ctx, m.Backend)
	if err != nil {
		return err
	}
	fresh, err := m.load(d)
	if err != nil {
		return err
	}
	_, err = m.install(d, rev, fresh)
	return err
}

// Refresh polls both sources, retaining last-good state on any error.
func (m *Manager) Refresh(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, rev, fresh, err := m.read(ctx)
	if err != nil {
		return nil, err
	}
	return m.install(d, rev, fresh)
}

// Status returns a copy of the last observed authority and conflicts.
func (m *Manager) Status() Status { _, status := m.Snapshot(); return status }

// Snapshot returns the configuration and its ownership revision together.
func (m *Manager) Snapshot() (*config.Config, Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Reload.Current(), Status{Sections: maps.Clone(m.status.Sections), Conflicts: slices.Clone(m.status.Conflicts)}
}

// Write persists a reviewed settings patch and installs it while holding the
// same lock as Refresh, so an old poll cannot undo a newly committed write.
func (m *Manager) Write(ctx context.Context, expected string, next *config.Config, paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, rev, fresh, err := m.read(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(paths, func(path string) bool { return strings.HasPrefix(path, "upstreams.") }) && m.statusFor(d, rev, fresh).Sections["upstreams"].Revision != expected {
		return ErrConflict
	}
	for _, path := range paths {
		section, _, _ := strings.Cut(path, ".")
		if m.owner(d, section) != UI {
			return ErrManaged
		}
	}
	if next.Store.Driver != "kube" {
		values := map[string]any{}
		flat := config.Flatten(next)
		for _, path := range paths {
			values[path] = flat[path]
		}
		// The file contains both the legacy writable settings and explicit ownership.
		if err := config.WriteSettingsFile(m.Reload.Current().SettingsFile, values); err != nil {
			return err
		}
	} else {
		if d.Owners == nil {
			d.Owners = map[string]string{}
		}
		if d.Values == nil {
			d.Values = map[string]any{}
		}
		d.Values["upstreams"] = upstreamValue(next)
		if _, err := m.Backend.Save(ctx, d, rev); err != nil {
			return err
		}
	}
	// Re-read the committed revision before publishing it to API readers.
	d, rev, fresh, err = m.read(ctx)
	if err != nil {
		return err
	}
	_, err = m.install(d, rev, fresh)
	return err
}

// Transition atomically adopts the current complete section or releases it
// after the operator has installed matching GitOps configuration.
func (m *Manager) Transition(ctx context.Context, section, action, expected string) error {
	if section != "upstreams" {
		return ErrUnsupported
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, rev, fresh, err := m.read(ctx)
	if err != nil {
		return err
	}
	status := m.statusFor(d, rev, fresh).Sections[section]
	if expected == "" || expected != status.Revision {
		return ErrConflict
	}
	if status.EnvLocked {
		return ErrEnvironment
	}
	if d.Owners == nil {
		d.Owners = map[string]string{}
	}
	if d.Values == nil {
		d.Values = map[string]any{}
	}
	switch action {
	case "adopt":
		if status.Owner == UI {
			return fmt.Errorf("%w: section is already UI-managed", ErrInvalid)
		}
		d.Owners[section], d.Values[section] = UI, upstreamValue(fresh)
	case "release":
		if status.Owner != UI {
			return fmt.Errorf("%w: section is already GitOps-managed", ErrInvalid)
		}
		// A release is a handoff, not an immediate reversion to stale Git values.
		git, err := config.Load(fresh.Source, m.Environ)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrHandoff, err)
		}
		if !reflect.DeepEqual(upstreamValue(git), upstreamValue(fresh)) {
			return ErrHandoff
		}
		d.Owners[section] = GitOps
		delete(d.Values, section)
	default:
		return fmt.Errorf("%w: unknown ownership action %q", ErrInvalid, action)
	}
	if _, err := m.Backend.Save(ctx, d, rev); err != nil {
		return err
	}
	d, rev, fresh, err = m.read(ctx)
	if err != nil {
		return err
	}
	_, err = m.install(d, rev, fresh)
	return err
}

// YAML exports complete Helm values. A proposed section is validated using the
// normal config schema and never persisted.
func (m *Manager) YAML(section string, proposed map[string]any) (string, error) {
	if section != "upstreams" {
		return "", ErrUnsupported
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.yamlLocked(section, proposed)
}

func (m *Manager) yamlLocked(section string, proposed map[string]any) (string, error) {
	c := m.Reload.Current()
	if proposed != nil {
		values, err := config.SectionValues(section, proposed)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		flat := config.Flatten(c)
		for _, key := range config.Schema {
			if !config.PublicClusterKey(key) || !strings.HasPrefix(key.Path, section+".") {
				continue
			}
			v, ok := values[key.Path]
			if !ok {
				v = key.Default
			}
			flat[key.Path] = v
		}
		var errDecode error
		c, errDecode = config.FromFlat(flat)
		if errDecode != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalid, errDecode)
		}
	}
	raw, err := yaml.Marshal(map[string]any{"config": map[string]any{"settings": map[string]any{section: upstreamValue(c)}}})
	return string(raw), err
}

// Export refreshes sources and returns YAML with the exact reviewed revision.
func (m *Manager) Export(ctx context.Context, section string, proposed map[string]any) (string, string, error) {
	if section != "upstreams" {
		return "", "", ErrUnsupported
	}
	if _, err := m.Refresh(ctx); err != nil {
		return "", "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	text, err := m.yamlLocked(section, proposed)
	return text, m.status.Sections[section].Revision, err
}

// SetNode serializes node-scope API edits with shared reloads. Only the reviewed
// node paths are changed, so a concurrent shared write cannot be overwritten.
func (m *Manager) SetNode(next *config.Config, paths []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.Reload.Current()
	flat := config.Flatten(cur)
	values := config.Flatten(next)
	for _, path := range paths {
		key, ok := config.Lookup(path)
		if !ok || key.Scope != config.ScopeNode {
			return ErrInvalid
		}
		flat[path] = values[path]
	}
	merged, err := config.FromFlat(flat)
	if err != nil {
		return err
	}
	merged.Source, merged.SettingsFile, merged.EnvLocked = cur.Source, cur.SettingsFile, cur.EnvLocked
	merged.DeclaredSections, merged.FileRevision = cur.DeclaredSections, cur.FileRevision
	m.Reload.Set(merged)
	return nil
}
