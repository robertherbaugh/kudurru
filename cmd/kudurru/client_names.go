package main

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/clientnames"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
)

func mountReversePTR(cfg config.ReversePTR, stage *identify.Stage) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	resolver, err := clientnames.New(cfg.Options())
	if err != nil {
		return nil, err
	}
	stage.FallbackHostnames = resolver
	return resolver.Run, nil
}
