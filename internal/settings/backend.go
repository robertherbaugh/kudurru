// Package settings coordinates explicit ownership of complete settings sections.
package settings

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/robertherbaugh/kudurru/internal/config"
	yaml "go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Section owners identify the authoritative source of complete settings values.
const (
	GitOps = "gitops"
	UI     = "ui"
	// EnvResource overrides the name of the API-owned ConfigMap in the release namespace.
	EnvResource     = "KUDURRU_SETTINGS_RESOURCE"
	DefaultResource = "kudurru-ui-settings"
)

// Ownership errors distinguish invalid requests, stale revisions, and unsafe handoffs.
var (
	ErrInvalid     = errors.New("invalid settings ownership request")
	ErrConflict    = errors.New("settings changed; reload and review before trying again")
	ErrManaged     = errors.New("settings section is GitOps-managed; adopt upstreams to edit through the UI/API")
	ErrEnvironment = errors.New("upstreams are locked by the environment; remove upstream environment overrides before changing ownership")
	ErrUnsupported = errors.New("only the upstreams section supports ownership changes")
	ErrHandoff     = errors.New("GitOps upstreams do not match the exported configuration; apply the YAML and wait for the settings volume to update before releasing ownership")
)

// Document keeps ownership and complete values together under one storage revision.
type Document struct {
	Owners map[string]string `json:"owners"`
	Values map[string]any    `json:"values"`
}

// Validate refuses unsupported ownership and invalid section values.
func (d Document) Validate() error {
	for section, owner := range d.Owners {
		if section != "upstreams" {
			return ErrUnsupported
		}
		if owner != GitOps && owner != UI {
			return fmt.Errorf("settings: invalid owner %q", owner)
		}
		if owner == UI && d.Values[section] == nil {
			return errors.New("settings: UI ownership requires complete section values")
		}
	}
	for section := range d.Values {
		if section != "upstreams" || d.Owners[section] != UI {
			return errors.New("settings: values require UI ownership")
		}
	}
	if d.Owners["upstreams"] == UI {
		section, ok := d.Values["upstreams"].(map[string]any)
		if !ok {
			return errors.New("settings: upstream values must be an object")
		}
		if _, err := config.SectionValues("upstreams", section); err != nil {
			return err
		}
		if _, err := config.LoadWithSections("", nil, func(*config.Config) (map[string]any, error) { return d.Sections(), nil }); err != nil {
			return err
		}
	}
	return nil
}

// Sections returns only complete UI-owned sections for configuration loading.
func (d Document) Sections() map[string]any {
	out := map[string]any{}
	for section, owner := range d.Owners {
		if owner == UI {
			out[section] = d.Values[section]
		}
	}
	return out
}

// Backend provides uncached reads and compare-and-swap writes. Empty revision
// means the resource does not exist; callers must never retry a stale write.
type Backend interface {
	Read(context.Context) (Document, string, error)
	Save(context.Context, Document, string) (string, error)
}

// KubeBackend uses a ConfigMap absent from Helm's rendered resources.
type KubeBackend struct {
	Maps coreclient.ConfigMapInterface
	Name string
}

func (b KubeBackend) Read(ctx context.Context) (Document, string, error) {
	cm, err := b.Maps.Get(ctx, b.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Document{}, "", nil
	}
	if err != nil {
		return Document{}, "", err
	}
	var d Document
	if err := json.Unmarshal([]byte(cm.Data["settings.json"]), &d); err != nil {
		return d, "", fmt.Errorf("UI settings: %w", err)
	}
	return d, cm.ResourceVersion, d.Validate()
}

// Save atomically replaces ownership and values using the ConfigMap revision.
func (b KubeBackend) Save(ctx context.Context, d Document, revision string) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: b.Name, ResourceVersion: revision}, Data: map[string]string{"settings.json": string(raw)}}
	if revision == "" {
		cm, err = b.Maps.Create(ctx, cm, metav1.CreateOptions{})
	} else {
		cm, err = b.Maps.Update(ctx, cm, metav1.UpdateOptions{})
	}
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	return cm.ResourceVersion, nil
}

// FileBackend retains the existing Docker settings file format; ownership and
// complete section values are written in the same atomic rename. The server's
// SQLite process lock and Manager mutex serialize writers.
type FileBackend struct{ Path string }

func (b FileBackend) Read(_ context.Context) (Document, string, error) {
	raw, err := os.ReadFile(b.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Document{}, "", nil
	}
	if err != nil {
		return Document{}, "", err
	}
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return Document{}, "", err
	}
	d := Document{Owners: map[string]string{}, Values: map[string]any{}}
	if owners, ok := tree["settingsOwnership"].(map[string]any); ok {
		for section, owner := range owners {
			s, ok := owner.(string)
			if !ok {
				return d, "", errors.New("settings: ownership must be gitops or ui")
			}
			d.Owners[section] = s
			if s == UI {
				d.Values[section] = tree[section]
			}
		}
	} else if tree["settingsOwnership"] != nil {
		return d, "", errors.New("settings: invalid ownership metadata")
	}
	return d, fmt.Sprintf("%x", sha256.Sum256(raw)), d.Validate()
}

// Save atomically persists ownership and values if the file revision still matches.
func (b FileBackend) Save(ctx context.Context, d Document, revision string) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	_, current, err := b.Read(ctx)
	if err != nil {
		return "", err
	}
	if current != revision {
		return "", ErrConflict
	}
	values := map[string]any{}
	for section, value := range d.Sections() {
		raw, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		var tree map[string]any
		if err := json.Unmarshal(raw, &tree); err != nil {
			return "", err
		}
		// Convert section values to the schema's dotted leaves.
		for _, key := range config.Schema {
			if len(key.Path) > len(section) && key.Path[:len(section)+1] == section+"." {
				v := any(tree)
				for _, part := range splitPath(key.Path[len(section)+1:]) {
					m, ok := v.(map[string]any)
					if !ok {
						v = nil
						break
					}
					v = m[part]
				}
				values[key.Path] = v
			}
		}
	}
	if err := config.WriteSettingsFileWithOwners(b.Path, values, d.Owners); err != nil {
		return "", err
	}
	_, rev, err := b.Read(ctx)
	return rev, err
}
