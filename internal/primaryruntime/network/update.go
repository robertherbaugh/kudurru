// Package network adapts committed-primary commands to DNS. Root supplies
// native verification, complete-cut selection and current writer publication.
package network

import (
	"errors"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
)

// CaptureUpdate selects the exact canonical apex from the request's complete
// cut and issues root's private native proof. It cannot fall back to live data.
type CaptureUpdate func(*pipeline.Request, string) (pr.VerifiedUpdate, error)

// UpdateReject carries an explicit root selection failure, never wire success.
type UpdateReject struct {
	Rcode int
	Err   error
}

func (r *UpdateReject) Error() string { return "primary network: UPDATE capture rejected" }
func (r *UpdateReject) Unwrap() error { return r.Err }

// UpdateOptions requires a committed runtime and a root capture callback.
// Observe receives internal diagnostics; those fields never appear on the wire.
type UpdateOptions struct {
	Runtime *pr.Runtime
	Capture CaptureUpdate
	Limits  dynamicupdate.Limits
	Observe func(pr.UpdateOutcome)
}

// NewUpdate returns authoritative's opcode callback. Missing capture disables
// UPDATE. Reply signing remains the native listener's responsibility.
func NewUpdate(o UpdateOptions) (pipeline.Next, error) {
	if o.Runtime == nil {
		return nil, pr.ErrInvalid
	}
	o.Limits = o.Limits.Defaults()
	return func(req *pipeline.Request) (*pipeline.Response, error) {
		if req == nil || req.Msg == nil {
			return nil, pr.ErrInvalid
		}
		reply := func(code int) (*pipeline.Response, error) {
			return pipeline.Reply(req, code, pipeline.StatusAuthoritative), nil
		}
		if req.Client.Synthetic || (req.Client.Proto != pipeline.ProtoUDP && req.Client.Proto != pipeline.ProtoTCP && req.Client.Proto != pipeline.ProtoDoT) {
			return reply(dns.RcodeRefused)
		}
		tx, protocol := dynamicupdate.Parse(req.Msg, o.Limits)
		if protocol != nil {
			return reply(protocol.Rcode)
		}
		if req.Ctx == nil || req.Ctx.Err() != nil || o.Capture == nil {
			return reply(dns.RcodeServerFailure)
		}
		verified, err := o.Capture(req, tx.Apex())
		if err != nil {
			code := dns.RcodeServerFailure
			var rejected *UpdateReject
			var pe *dynamicupdate.ProtocolError
			if errors.As(err, &rejected) && rejected != nil && rejected.Rcode > 0 && rejected.Rcode <= 15 {
				code = rejected.Rcode
			} else if errors.As(err, &pe) && pe != nil && pe.Rcode > 0 && pe.Rcode <= 15 {
				code = pe.Rcode
			}
			return reply(code)
		}
		if verified.Zone.Apex != tx.Apex() {
			return reply(dns.RcodeServerFailure)
		}
		out := o.Runtime.Apply(req.Ctx, verified)
		if out.Rcode < 0 || out.Rcode > 15 || (out.Rcode == dns.RcodeSuccess && (!out.Published || out.Err != nil)) {
			out.Rcode = dns.RcodeServerFailure
		}
		if o.Observe != nil {
			observe(o.Observe, out)
		}
		return reply(out.Rcode)
	}, nil
}

// Diagnostics must not change an already determined DNS outcome.
func observe(fn func(pr.UpdateOutcome), out pr.UpdateOutcome) {
	defer func() { _ = recover() }()
	fn(out)
}
