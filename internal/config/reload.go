package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"go.yaml.in/yaml/v3"
)

// EnvSettingsFile names the settings file (D25) and takes precedence over
// the settingsFile key of the node file.
const EnvSettingsFile = EnvPrefix + "SETTINGS_FILE"

// DefaultSettingsPoll is how often WatchSettings re-reads the settings file
// besides fsnotify (ConfigMap volume updates, network filesystems).
const DefaultSettingsPoll = 10 * time.Second

// settingsFilePath resolves the settings file: KUDURRU_SETTINGS_FILE, else
// the settingsFile key (environment override or file value).
func settingsFilePath(k *koanf.Koanf, overrides map[string]any, environ []string) string {
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok && name == EnvSettingsFile {
			return strings.TrimSpace(value)
		}
	}
	if v, ok := overrides["settingsFile"].(string); ok {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(k.String("settingsFile"))
}

// ReadSettingsFile reads the cluster-scope settings layer at path as dotted
// schema paths. A missing or empty file is an empty layer (the API's
// settings writer creates it). Unknown keys and node-scope keys are
// errors: the file only carries the Kudurru singleton.
func ReadSettingsFile(path string) (map[string]any, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the operator names the file (settingsFile, KUDURRU_SETTINGS_FILE)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: settings file: %w", err)
	}
	return parseSettings(path, b)
}

func parseSettings(path string, b []byte) (map[string]any, error) {
	var tree map[string]any
	if err := yaml.Unmarshal(b, &tree); err != nil {
		return nil, fmt.Errorf("config: settings file %s: %w", path, err)
	}
	out := map[string]any{}
	if err := settingsLeaves("", tree, out); err != nil {
		return nil, fmt.Errorf("config: settings file %s: %w", path, err)
	}
	return out, nil
}

// settingsLeaves flattens tree onto schema paths, refusing unknown and
// node-scope keys.
func settingsLeaves(prefix string, tree map[string]any, out map[string]any) error {
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs []error
	for _, k := range keys {
		v := tree[k]
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if key, ok := Lookup(p); ok {
			if key.Scope != ScopeCluster {
				errs = append(errs, fmt.Errorf("%s is a node-scope key; set it in the node file", p))
				continue
			}
			out[p] = v
			continue
		}
		sub, ok := v.(map[string]any)
		if !ok || !hasPrefix(p+".") {
			errs = append(errs, fmt.Errorf("%s: unknown settings key", p))
			continue
		}
		if err := settingsLeaves(p, sub, out); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func hasPrefix(prefix string) bool {
	for _, k := range Schema {
		if strings.HasPrefix(k.Path, prefix) {
			return true
		}
	}
	return false
}

// WriteSettingsFile sets the given cluster-scope keys (dotted schema paths)
// in the settings file at path, keeping the keys it does not touch, and
// replaces the file atomically (write to a temporary file, fsync, rename).
// Comments in the file are not preserved.
func WriteSettingsFile(path string, values map[string]any) error {
	cur, err := ReadSettingsFile(path)
	if err != nil {
		return err
	}
	for p, v := range values {
		k, ok := Lookup(p)
		if !ok {
			return fmt.Errorf("config: %s: unknown settings key", p)
		}
		if k.Scope != ScopeCluster {
			return fmt.Errorf("config: %s is a node-scope key and does not belong in the settings file", p)
		}
		cur[p] = v
	}
	tree := map[string]any{}
	for p, v := range cur {
		setPath(tree, p, v)
	}
	var buf bytes.Buffer
	buf.WriteString("# kudurru cluster-scope settings (D25); written by PATCH /api/v1/settings.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(tree); err != nil {
		return fmt.Errorf("config: encode settings: %w", err)
	}
	if err := enc.Close(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("config: settings file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.yaml")
	if err != nil {
		return fmt.Errorf("config: settings file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: settings file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: settings file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: settings file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil { //nolint:gosec // G302: group-readable like the node file
		return fmt.Errorf("config: settings file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("config: settings file: %w", err)
	}
	return nil
}

func setPath(tree map[string]any, p string, v any) {
	segs := strings.Split(p, ".")
	cur := tree
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

// Flatten renders cfg as dotted schema path → value, using the JSON form of
// the config structs (whose tags equal the koanf keys). Keys the JSON form
// omits get their schema default.
func Flatten(cfg *Config) map[string]any {
	out := make(map[string]any, len(Schema))
	raw, err := json.Marshal(cfg)
	var tree map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &tree)
	}
	for _, k := range Schema {
		if v, ok := lookupPath(tree, k.Path); ok && err == nil {
			out[k.Path] = v
		} else {
			out[k.Path] = k.Default
		}
	}
	return out
}

func lookupPath(tree map[string]any, p string) (any, bool) {
	var cur any = tree
	for _, seg := range strings.Split(p, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// FromFlat decodes a flat key map (Flatten's form) into a validated Config.
// EnvLocked, Source and the node name default are left to the caller.
func FromFlat(flat map[string]any) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	cfg.Upstreams.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	return &cfg, nil
}

// MergeCluster returns cur with every cluster-scope key taken from fresh;
// node-scope keys (restart-bound, or changed in memory through the API)
// keep cur's values. Settings reloads use it so that a node-file edit
// never half-applies.
func MergeCluster(cur, fresh *Config) (*Config, error) {
	flat := Flatten(cur)
	ff := Flatten(fresh)
	for _, k := range Schema {
		if k.Scope == ScopeCluster {
			flat[k.Path] = ff[k.Path]
		}
	}
	next, err := FromFlat(flat)
	if err != nil {
		return nil, err
	}
	next.EnvLocked, next.Source, next.SettingsFile = cur.EnvLocked, cur.Source, cur.SettingsFile
	next.Node.Name = cur.Node.Name
	return next, nil
}

// Changed lists the schema paths whose values differ between a and b,
// sorted.
func Changed(a, b *Config) []string {
	fa, fb := Flatten(a), Flatten(b)
	var out []string
	for _, k := range Schema {
		if !reflect.DeepEqual(fa[k.Path], fb[k.Path]) {
			out = append(out, k.Path)
		}
	}
	return out
}

// ChangedUnder reports whether any path in changed is prefix or below it
// ("cache" matches cache.maxEntries).
func ChangedUnder(changed []string, prefixes ...string) bool {
	for _, c := range changed {
		for _, p := range prefixes {
			if c == p || strings.HasPrefix(c, p+".") {
				return true
			}
		}
	}
	return false
}

// Reloadable holds the effective configuration and notifies subscribers
// when it changes (settings hot reload, D25; PATCH /settings). Readers
// call Current on every use instead of keeping a *Config.
type Reloadable struct {
	cur atomic.Pointer[Config]

	mu   sync.Mutex // serialises Set and the subscriber list
	subs map[int]func(old, next *Config, changed []string)
	next int
}

// NewReloadable starts with cfg.
func NewReloadable(cfg *Config) *Reloadable {
	r := &Reloadable{subs: map[int]func(old, next *Config, changed []string){}}
	r.cur.Store(cfg)
	return r
}

// Current returns the effective configuration. Treat it as read-only.
func (r *Reloadable) Current() *Config { return r.cur.Load() }

// Subscribe registers fn, called synchronously by Set (in registration
// order, one Set at a time) with the previous and new configuration and the
// changed schema paths. fn must not call Set. The returned func removes it.
func (r *Reloadable) Subscribe(fn func(old, next *Config, changed []string)) (cancel func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.next
	r.next++
	r.subs[id] = fn
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.subs, id)
	}
}

// Set installs next and notifies the subscribers when anything changed. It
// returns the changed paths.
func (r *Reloadable) Set(next *Config) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.cur.Load()
	changed := Changed(old, next)
	r.cur.Store(next)
	if len(changed) == 0 {
		return nil
	}
	ids := make([]int, 0, len(r.subs))
	for id := range r.subs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		r.subs[id](old, next, changed)
	}
	return changed
}

// Reload re-reads the node file, the settings file and the environment and
// installs the cluster-scope keys (MergeCluster). Node-file edits of
// node-scope keys still need a restart. It returns the changed paths.
func (r *Reloadable) Reload(environ []string) ([]string, error) {
	cur := r.Current()
	fresh, err := Load(cur.Source, environ)
	if err != nil {
		return nil, err
	}
	next, err := MergeCluster(cur, fresh)
	if err != nil {
		return nil, err
	}
	return r.Set(next), nil
}

// WatchOptions configures WatchSettings.
type WatchOptions struct {
	// Environ is the process environment Load sees (os.Environ()).
	Environ []string
	// Poll is the re-read interval besides fsnotify; 0 means
	// DefaultSettingsPoll, negative disables polling.
	Poll time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// OnReload, when set, is told the outcome of every reload attempt
	// (metrics, audit).
	OnReload func(changed []string, err error)
}

// WatchSettings watches the settings file of r.Current() (fsnotify on its
// directory, which covers Kubernetes ConfigMap symlink swaps and editors
// that rename, plus polling) and calls r.Reload when its content changes.
// An invalid file is logged and ignored: the last good configuration stays.
// It returns when ctx is done; without a settings file it just waits.
func WatchSettings(ctx context.Context, r *Reloadable, opts WatchOptions) error {
	path := r.Current().SettingsFile
	if path == "" {
		<-ctx.Done()
		return nil
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	log := opts.Logger.With("component", "settings", "file", path)
	digest := func() [32]byte {
		b, err := os.ReadFile(path) //nolint:gosec // operator-named settings file
		if err != nil {
			return [32]byte{}
		}
		return sha256.Sum256(b)
	}
	last := digest()

	var events <-chan fsnotify.Event
	var errs <-chan error
	if w, err := fsnotify.NewWatcher(); err != nil {
		log.Warn("fsnotify unavailable; settings reload falls back to polling", "err", err)
	} else {
		defer func() { _ = w.Close() }()
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			log.Warn("settings directory unavailable", "err", err)
		}
		if err := w.Add(dir); err != nil {
			log.Warn("cannot watch the settings directory; relying on polling", "dir", dir, "err", err)
		}
		events, errs = w.Events, w.Errors
	}
	poll := opts.Poll
	if poll == 0 {
		poll = DefaultSettingsPoll
	}
	var tick <-chan time.Time
	if poll > 0 {
		t := time.NewTicker(poll)
		defer t.Stop()
		tick = t.C
	}
	const debounce = 200 * time.Millisecond
	var fire <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	check := func() {
		d := digest()
		if d == last {
			return
		}
		changed, err := r.Reload(opts.Environ)
		if opts.OnReload != nil {
			opts.OnReload(changed, err)
		}
		if err != nil {
			log.Error("settings file not applied; keeping the previous settings", "err", err)
			last = d // do not retry the same bad content every poll
			return
		}
		last = d
		if len(changed) > 0 {
			log.Info("settings reloaded", "changed", changed)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				timer.Reset(debounce)
			}
			fire = timer.C
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			log.Warn("settings watcher error", "err", err)
		case <-fire:
			fire = nil
			check()
		case <-tick:
			check()
		}
	}
}
