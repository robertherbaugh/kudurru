package main

import (
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
)

// This receipt is issued only after the actual root-bound UPDATE adapter
// returns. It authorizes its exact empty acknowledgment, never DNS data or a
// transfer. The adapter already requires exact publication before NOERROR;
// a later output failure does not undo a durable commit or replay the command.
type rootUpdateAcknowledgment struct {
	request  pipeline.Request
	response *pipeline.Response
	message  *dns.Msg
	header   dns.MsgHdr
	question dns.Question
	writer   primaryWriterBinding
	owner    *servingbuild.RuntimeOwner
	proof    *dnsserver.VerifiedTSIG
}

func (o *rootAnchorOutput) completeUpdate(req *pipeline.Request, response *pipeline.Response, writer *primaryWriter) {
	if o == nil || req == nil || req.Ctx == nil || req.Msg == nil || response == nil || response.Msg == nil {
		return
	}
	if o.ack != nil || o.state.Load() != anchorOutputReady {
		o.state.Store(anchorOutputTerminal)
		return
	}
	seal, err := servingdns.Component[*recursiveRootSeal](req, recursiveRootComponent)
	if err != nil || seal != o.seal {
		return
	}
	owner, err := servingdns.Component[*servingbuild.RuntimeOwner](req, "runtime")
	if err != nil {
		return
	}
	binding, ok := req.Ctx.Value(primaryWriterBindingKey{}).(primaryWriterBinding)
	if !ok || binding.owner != writer || !writer.current(binding.term) {
		return
	}
	proof, ok := dnsserver.VerifiedTSIGFor(req, time.Now())
	if !ok || len(req.Msg.Question) != 1 {
		return
	}
	request := *req
	request.Msg = req.Msg.Copy()
	ack := &rootUpdateAcknowledgment{request: request, response: response, message: response.Msg,
		header: response.Msg.MsgHdr, question: req.Msg.Question[0], writer: binding, owner: owner, proof: proof}
	if !ack.matches(response) || !ack.current() {
		return
	}
	o.ack = ack
}

func (a *rootUpdateAcknowledgment) matches(response *pipeline.Response) bool {
	if a == nil || response != a.response || response == nil || response.Msg != a.message || response.Transfer != nil {
		return false
	}
	m := response.Msg
	return m != nil && m.MsgHdr == a.header && m.Response && m.Opcode == dns.OpcodeUpdate &&
		m.Id == a.request.Msg.Id && m.Rcode >= 0 && m.Rcode <= 15 &&
		len(m.Question) == 1 && m.Question[0] == a.question &&
		len(m.Answer) == 0 && len(m.Ns) == 0 && len(m.Extra) == 0
}

func (a *rootUpdateAcknowledgment) current() bool {
	if a == nil || a.request.Ctx.Err() != nil || !a.writer.owner.current(a.writer.term) || !a.owner.DependenciesCurrent() {
		return false
	}
	proof, ok := dnsserver.VerifiedTSIGFor(&a.request, time.Now())
	if !ok || proof != a.proof {
		return false
	}
	_, keys, _ := a.owner.NotifyKeys()
	identity, ok := keys.Identity(proof.Identity().Key)
	return ok && identity == proof.Identity() && a.matches(a.response)
}

// BindResponse runs after every pipeline stage, before packing/signing. A
// substituted response or added data cannot use the command acknowledgment.
func (o *rootAnchorOutput) BindResponse(response *pipeline.Response) error {
	if o == nil || o.state.Load() != anchorOutputReady {
		return serving.ErrDependency
	}
	if o.ack == nil {
		if o.bound {
			o.state.Store(anchorOutputTerminal)
			return serving.ErrDependency
		}
		o.bound = true
		// These are internal pipeline provenance fields, never wire AA/AD or
		// client claims. Local/authoritative/mixed data stays generation-bound.
		o.retainedDNS = o.query && retainedDNSResponse(response)
		// Local, authoritative and mixed answers are bound to the served
		// publication rather than to the latest commit (D378). A transfer
		// or any other opcode stays bound to full data currency.
		o.servedDNS = !o.retainedDNS && o.query && servedDNSResponse(response)
		return nil
	}
	if o.bound || !o.ack.matches(response) || !o.ack.current() {
		o.state.Store(anchorOutputTerminal)
		return serving.ErrDependency
	}
	o.bound = true
	return nil
}

func servedDNSResponse(response *pipeline.Response) bool {
	return response != nil && response.Msg != nil && response.Msg.Opcode == dns.OpcodeQuery && response.Transfer == nil
}

func retainedDNSResponse(response *pipeline.Response) bool {
	if response == nil || response.Msg == nil || response.Msg.Opcode != dns.OpcodeQuery || response.Transfer != nil {
		return false
	}
	if response.Origin == pipeline.OriginForwarded || response.Origin == pipeline.OriginSynthetic && response.Status.Class() == pipeline.ClassBlocked {
		return true
	}
	// Only ingress produces SpecialDomain: node health and CHAOS identity
	// replies contain no canonical zone data. Keep the last complete route's
	// liveness available while a settings successor cannot be prepared.
	if response.Status == pipeline.StatusSpecialDomain && response.Origin == pipeline.OriginUnknown {
		return true
	}
	// Expired/unavailable local data may report its empty refusal. This gives
	// no local answer, delegation, denial proof or transfer generation credit.
	// A query whose upstream was asked and failed or timed out reports the
	// same empty refusal: its forwarded answer would have been retained above,
	// and withholding the failure only leaves the client to time out. Only the
	// upstream stage's own unattributed failure with UpstreamContacted
	// qualifies (D377). A refusal the node produced itself (failed serving
	// capture, refused native recursion, indeterminate DNSSEC validation,
	// a failed rewrite target or stub zone) and anything that claims
	// authoritative, local, mixed or synthesized data stays generation-bound.
	m := response.Msg
	switch response.Status {
	case pipeline.StatusServfailUnsynced:
	case pipeline.StatusServfailUpstream, pipeline.StatusTimeoutUpstream:
		if response.Origin != pipeline.OriginUnknown || !response.UpstreamContacted {
			return false
		}
	default:
		return false
	}
	if m.Rcode != dns.RcodeServerFailure || m.Authoritative || len(m.Answer) != 0 || len(m.Ns) != 0 {
		return false
	}
	for _, rr := range m.Extra {
		if _, ok := rr.(*dns.OPT); !ok {
			return false
		}
	}
	return true
}
