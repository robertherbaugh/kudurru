package main

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

func kubeStartupTimeout(cfg *config.Config) time.Duration {
	if d := cfg.Store.Kube.StartupTimeout.Std(); d > 0 {
		return d
	}
	return kube.DefaultStartupTimeout
}
