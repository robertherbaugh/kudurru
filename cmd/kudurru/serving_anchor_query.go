package main

import (
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func captureRootAnchorQueryOutput(req *pipeline.Request) (api.DNSQueryEmission, error) {
	return captureRootAnchorOutput(req)
}
