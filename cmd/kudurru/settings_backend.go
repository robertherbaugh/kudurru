package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/settings"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

func settingsBackend(cfg *config.Config) (settings.Backend, error) {
	if cfg.Store.Driver != "kube" {
		if cfg.SettingsFile == "" {
			return nil, nil
		}
		return settings.FileBackend{Path: cfg.SettingsFile}, nil
	}
	restConfig, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, err
	}
	// Bound startup and poll reads during an apiserver outage.
	restConfig.Timeout = config.DefaultSettingsPoll
	core, err := coreclient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	name := os.Getenv(settings.EnvResource)
	if name == "" {
		name = settings.DefaultResource
	}
	backend := settings.KubeBackend{Maps: core.ConfigMaps(kube.PodNamespace()), Name: name}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(restConfig.Host+"/"+kube.PodNamespace()+"/"+name)))
	return settings.CachedBackend{Backend: backend, Path: filepath.Join(cfg.Node.DataDir, "ui-settings-"+identity[:16]+".json")}, nil
}
