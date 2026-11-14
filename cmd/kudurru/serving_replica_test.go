package main

import (
	"context"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// captureServingReplica retains the receiver before a fixture acquires its
// source image. It never accepts an image or renews a previous operation.
func captureServingReplica(t *testing.T, receiver *sqlite.Store) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	if _, err := receiver.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	captured, err := receiver.CaptureAnchorInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return captured
}
