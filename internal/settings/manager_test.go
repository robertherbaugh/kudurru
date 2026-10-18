package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

type memoryBackend struct {
	mu      sync.Mutex
	raw     []byte
	version int
	failure error
}

func (b *memoryBackend) Read(context.Context) (Document, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var d Document
	if len(b.raw) > 0 {
		_ = json.Unmarshal(b.raw, &d)
	}
	return d, fmt.Sprint(b.version), b.failure
}
func (b *memoryBackend) Save(_ context.Context, d Document, rev string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failure != nil {
		return "", b.failure
	}
	if rev != fmt.Sprint(b.version) {
		return "", ErrConflict
	}
	b.raw, _ = json.Marshal(d)
	b.version++
	return fmt.Sprint(b.version), nil
}

func newManager(t *testing.T, b Backend, node string, env []string) *Manager {
	t.Helper()
	c, err := config.LoadWithSections(node, env, func(*config.Config) (map[string]any, error) { d, _, e := b.Read(t.Context()); return d.Sections(), e })
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Backend: b, Reload: config.NewReloadable(c), Environ: env}
	if _, err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	return m
}
func revision(m *Manager) string { return m.Status().Sections["upstreams"].Revision }
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipReplicasRestartAndHandoff(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "node.yaml")
	file := filepath.Join(dir, "settings.yaml")
	write(t, node, "store: {driver: kube}\nsettingsFile: "+file+"\n")
	write(t, file, "upstreams:\n  groups:\n    lan: {servers: ['192.0.2.1:53']}\n  conditional: {'corp.example.': lan}\n  bootstrap: ['9.9.9.9:53']\ncache: {maxEntries: 42}\n")
	backend := &memoryBackend{}
	a := newManager(t, backend, node, nil)
	b := newManager(t, backend, node, nil)
	before := a.Reload.Current().Upstreams
	next := *a.Reload.Current()
	next.Upstreams.Bootstrap = []string{"8.8.8.8:53"}
	if err := a.Write(t.Context(), revision(a), &next, []string{"upstreams.bootstrap"}); !errors.Is(err, ErrManaged) {
		t.Fatalf("GitOps write: %v", err)
	}
	stale := revision(b)
	if err := a.Transition(t.Context(), "upstreams", "adopt", revision(a)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, a.Reload.Current().Upstreams) {
		t.Fatal("adoption changed routing")
	}
	if len(a.Status().Conflicts) != 1 {
		t.Fatal("missing conflict")
	}
	if err := b.Transition(t.Context(), "upstreams", "adopt", stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale adoption: %v", err)
	}
	next = *a.Reload.Current()
	next.Upstreams.Bootstrap = []string{"8.8.8.8:53"}
	if err := a.Write(t.Context(), revision(a), &next, []string{"upstreams.bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Reload.Current().Upstreams, b.Reload.Current().Upstreams) {
		t.Fatal("replicas disagree")
	}
	// A Helm reconciliation replaces file values, never the API-owned section.
	write(t, file, "upstreams: {groups: {default: {servers: ['192.0.2.99:53']}}}\ncache: {maxEntries: 84}\n")
	if _, err := a.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.Reload.Current().Cache.MaxEntries != 84 || a.Reload.Current().Upstreams.Bootstrap[0] != "8.8.8.8:53" || a.Reload.Current().Upstreams.Conditional["corp.example."] != "lan" {
		t.Fatal("Helm update overwrote UI section or failed to update other sections")
	}
	restarted := newManager(t, backend, node, nil)
	if !reflect.DeepEqual(a.Reload.Current().Upstreams, restarted.Reload.Current().Upstreams) {
		t.Fatal("restart lost UI state")
	}
	if err := a.Transition(t.Context(), "upstreams", "release", revision(a)); !errors.Is(err, ErrHandoff) {
		t.Fatalf("unsafe release: %v", err)
	}
	exported, err := a.YAML("upstreams", nil)
	if err != nil {
		t.Fatal(err)
	}
	if exported == "" {
		t.Fatal("empty export")
	}
	// Write exactly the exported section as the operator would through Helm.
	raw, _ := json.Marshal(map[string]any{"upstreams": upstreamValue(a.Reload.Current()), "cache": map[string]any{"maxEntries": 84}})
	write(t, file, string(raw))
	if _, err := a.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	before = a.Reload.Current().Upstreams
	if err := a.Transition(t.Context(), "upstreams", "release", revision(a)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, a.Reload.Current().Upstreams) || a.Status().Sections["upstreams"].Owner != GitOps {
		t.Fatal("release changed routing or failed to hand off")
	}
	if _, err := b.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.Status().Sections["upstreams"].Owner != GitOps {
		t.Fatal("release did not propagate")
	}
}

func TestOwnedSectionReplacesStaleInvalidFileAndRejectsEnvironmentAdoption(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "node.yaml")
	file := filepath.Join(dir, "settings.yaml")
	write(t, node, "store: {driver: kube}\nsettingsFile: "+file+"\n")
	backend := &memoryBackend{}
	m := newManager(t, backend, node, nil)
	if err := m.Transition(t.Context(), "upstreams", "adopt", revision(m)); err != nil {
		t.Fatal(err)
	}
	for _, ignored := range []string{
		"upstreams: {groups: {default: {mode: invalid}}}\n",
		"upstreams: {unknown: true}\n",
		"upstreams: invalid\n",
	} {
		write(t, file, ignored)
		if _, err := m.Refresh(t.Context()); err != nil {
			t.Fatalf("ignored UI-owned file section %q: %v", ignored, err)
		}
		_ = newManager(t, backend, node, nil)
	}
	write(t, file, "upstreams: invalid\ncache: {unknown: true}\n")
	if _, err := m.Refresh(t.Context()); err == nil {
		t.Fatal("invalid GitOps-owned section was ignored")
	}
	write(t, file, "upstreams: {ecs: {mode: strip}}\n")
	other := &memoryBackend{}
	env := []string{"KUDURRU_UPSTREAMS__ECS__MODE=forward"}
	locked := newManager(t, other, node, env)
	if err := locked.Transition(t.Context(), "upstreams", "adopt", revision(locked)); !errors.Is(err, ErrEnvironment) {
		t.Fatalf("environment adoption: %v", err)
	}
}

func TestFileOwnershipAndLegacyWrites(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.yaml")
	env := []string{config.EnvSettingsFile + "=" + file}
	backend := FileBackend{Path: file}
	m := newManager(t, backend, "", env)
	if m.Status().Sections["upstreams"].Owner != UI {
		t.Fatal("Docker compatibility")
	}
	next := *m.Reload.Current()
	next.Cache.MaxEntries = 42
	if err := m.Write(t.Context(), revision(m), &next, []string{"cache.maxEntries"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(t.Context(), "upstreams", "release", revision(m)); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(t.Context(), "upstreams", "adopt", revision(m)); err != nil {
		t.Fatal(err)
	}
	next = *m.Reload.Current()
	next.Upstreams.Bootstrap = []string{"9.9.9.9:53"}
	if err := m.Write(t.Context(), revision(m), &next, []string{"upstreams.bootstrap"}); err != nil {
		t.Fatal(err)
	}
	restarted := newManager(t, backend, "", env)
	if restarted.Reload.Current().Cache.MaxEntries != 42 || restarted.Reload.Current().Upstreams.Bootstrap[0] != "9.9.9.9:53" || restarted.Status().Sections["upstreams"].Owner != UI {
		t.Fatal("file persistence lost values or ownership")
	}
}

func TestFileNonUpstreamWritePreservesNewerUpstreams(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.yaml")
	m := newManager(t, FileBackend{Path: file}, "", []string{config.EnvSettingsFile + "=" + file})
	reviewed := revision(m)
	cache := *m.Reload.Current()
	cache.Cache.MaxEntries = 42
	upstreams := *m.Reload.Current()
	upstreams.Upstreams.Bootstrap = []string{"192.0.2.1:53"}
	if err := m.Write(t.Context(), reviewed, &upstreams, []string{"upstreams.bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if revision(m) == reviewed {
		t.Fatal("upstream edit did not change its revision")
	}
	if err := m.Write(t.Context(), reviewed, &cache, []string{"upstreams.bootstrap", "cache.maxEntries"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("mixed stale upstream edit: %v", err)
	}
	if err := m.Write(t.Context(), reviewed, &cache, []string{"cache.maxEntries"}); err != nil {
		t.Fatalf("unrelated cache edit rejected: %v", err)
	}
	if m.Reload.Current().Cache.MaxEntries != 42 || m.Reload.Current().Upstreams.Bootstrap[0] != "192.0.2.1:53" {
		t.Fatal("cache edit lost the newer upstream configuration")
	}
}

func TestSharedSettingsPollWithoutFileAndLastGood(t *testing.T) {
	backend := &memoryBackend{}
	env := []string{"KUDURRU_STORE__DRIVER=kube"}
	a := newManager(t, backend, "", env)
	b := newManager(t, backend, "", env)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- config.WatchSettings(ctx, b.Reload, config.WatchOptions{Refresh: b.Refresh, Poll: 10 * time.Millisecond})
	}()
	if err := a.Transition(t.Context(), "upstreams", "adopt", revision(a)); err != nil {
		t.Fatal(err)
	}
	next := *a.Reload.Current()
	next.Upstreams.Bootstrap = []string{"9.9.9.9:53"}
	if err := a.Write(t.Context(), revision(a), &next, []string{"upstreams.bootstrap"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !reflect.DeepEqual(a.Reload.Current().Upstreams, b.Reload.Current().Upstreams) {
		if time.Now().After(deadline) {
			t.Fatal("poll did not propagate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.failure = errors.New("offline")
	backend.mu.Unlock()
	before := b.Reload.Current()
	if _, err := b.Refresh(t.Context()); err == nil {
		t.Fatal("missing read failure")
	}
	if b.Reload.Current() != before {
		t.Fatal("failed read changed last good settings")
	}
}

func TestOwnershipRevisionIsSharedAcrossNodeIdentity(t *testing.T) {
	dir := t.TempDir()
	aFile := filepath.Join(dir, "a.yaml")
	bFile := filepath.Join(dir, "b.yaml")
	write(t, aFile, "node: {name: a}\nlog: {level: debug}\nstore: {driver: kube}\n")
	write(t, bFile, "node: {name: b}\nlog: {level: info}\nstore: {driver: kube}\n")
	backend := &memoryBackend{}
	a := newManager(t, backend, aFile, nil)
	b := newManager(t, backend, bFile, []string{"KUDURRU_NODE__NAME=other"})
	if revision(a) != revision(b) {
		t.Fatal("node-local settings changed the shared section revision")
	}
	if err := a.Transition(t.Context(), "upstreams", "adopt", revision(b)); err != nil {
		t.Fatal(err)
	}
}

func TestProposedYAMLValidatesWithoutWriting(t *testing.T) {
	backend := &memoryBackend{}
	m := newManager(t, backend, "", []string{"KUDURRU_STORE__DRIVER=kube"})
	before := m.Reload.Current()
	rev := revision(m)
	if _, err := m.YAML("upstreams", map[string]any{"unknown": true}); err == nil {
		t.Fatal("unknown key accepted")
	}
	proposed := upstreamValue(before).(map[string]any)
	proposed["groups"] = map[string]any{"default": map[string]any{"mode": "recursive", "timeout": "4s"}}
	text, err := m.YAML("upstreams", proposed)
	if err != nil {
		t.Fatal(err)
	}
	if text == "" || revision(m) != rev || m.Reload.Current() != before || len(backend.raw) > 0 {
		t.Fatal("preview wrote settings")
	}
}

func TestNodeEditsPreserveConcurrentSharedChanges(t *testing.T) {
	backend := &memoryBackend{}
	m := newManager(t, backend, "", []string{"KUDURRU_STORE__DRIVER=kube"})
	node := *m.Reload.Current()
	node.Log.Level = "debug"
	if err := m.Transition(t.Context(), "upstreams", "adopt", revision(m)); err != nil {
		t.Fatal(err)
	}
	next := *m.Reload.Current()
	next.Upstreams.Bootstrap = []string{"192.0.2.1:53"}
	if err := m.Write(t.Context(), revision(m), &next, []string{"upstreams.bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetNode(&node, []string{"log.level"}); err != nil {
		t.Fatal(err)
	}
	if m.Reload.Current().Upstreams.Bootstrap[0] != "192.0.2.1:53" || m.Reload.Current().Log.Level != "debug" {
		t.Fatal("node edit overwrote shared settings")
	}
}
