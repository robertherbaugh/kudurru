package dnsserver

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Output owns the original request's final emission admission. Root composition
// captures its first attempt before signature verification or pipeline work.
// Emit consumes that attempt while fn synchronously completes actual writes.
// Next captures a subsequent transfer-frame attempt from the SAME original
// input and lifetime, before pulling that frame. A failed attempt is terminal.
type Output interface {
	Emit(fn func() error) error
	Next() error
}

// CaptureOutput binds output to an already owned complete request publication.
// It must neither capture a replacement publication nor extend the lifetime.
type CaptureOutput func(*pipeline.Request) (Output, error)

type transportOutputKey struct{}

type transportResponseKey struct{}

// Only the synchronous pipeline return fills this private carrier. It retains
// actual validation provenance through packing and final output admission.
type transportResponse struct{ security pipeline.Security }

func checkTransportResponse(ctx context.Context) error {
	response, _ := ctx.Value(transportResponseKey{}).(*transportResponse)
	if response != nil && !response.security.AvailableAt(time.Now()) {
		return errOriginalDeadline
	}
	return nil
}
