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

// ResponseOutput can bind a root-issued command acknowledgment to the exact
// synchronous pipeline result. An opcode or response code alone is not authority.
// A refused binding drops the response; it must not recapture an output owner.
type ResponseOutput interface {
	BindResponse(*pipeline.Response) error
}

// CapturedOutput returns only the original pre-verification output capability.
// Root command composition uses it to attach its private completion receipt.
func CapturedOutput(req *pipeline.Request) Output {
	if req == nil || req.Ctx == nil {
		return nil
	}
	output, _ := req.Ctx.Value(transportOutputKey{}).(Output)
	return output
}

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
