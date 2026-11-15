package dnsserver

import (
	"crypto/sha256"
	"crypto/sha512"
	"slices"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

const responsePaddingBlock = 468 // RFC 8467 section 4.1

// responsePadding is captured before the pipeline can adapt request EDNS.
// Padding is a transport response policy, never shared cache content.
type responsePadding struct {
	enabled bool
	limit   int
	do      bool
}

func paddingFor(r *dns.Msg, proto pipeline.Proto) responsePadding {
	switch proto {
	case pipeline.ProtoDoT, pipeline.ProtoDoH, pipeline.ProtoDoQ, pipeline.ProtoDoH3:
	default:
		return responsePadding{}
	}
	opt := r.IsEdns0()
	if opt == nil || opt.Version() != 0 {
		return responsePadding{}
	}
	for _, option := range opt.Option {
		if option.Option() == dns.EDNS0PADDING {
			// RFC 6891 treats advertised values below 512 as 512. RFC 7830
			// bounds padded messages by this value even on encrypted streams.
			return responsePadding{enabled: true, limit: max(dns.MinMsgSize, int(opt.UDPSize())), do: opt.Do()}
		}
	}
	return responsePadding{}
}

// apply returns a detached padded reply, or leaves an unpadded reply unchanged.
// It never changes the request, a cached answer, signed wire evidence, or the
// response's records to make padding fit. PADDING is the final EDNS option;
// transport length prefixes are excluded and the final TSIG trailer is counted.
func (p responsePadding) apply(m *dns.Msg) *dns.Msg {
	opt := m.IsEdns0()
	hasPadding := opt != nil && slices.ContainsFunc(opt.Option, isPaddingOption)
	if !p.enabled && !hasPadding {
		return m
	}
	// An already signed message is immutable. Native signing attaches an
	// empty TSIG stub and fills it only after this transport step.
	if tsig := m.IsTsig(); tsig != nil && tsig.MAC != "" {
		return m
	}
	out := m.Copy()
	opt = out.IsEdns0()
	if opt != nil {
		opt.Option = slices.DeleteFunc(opt.Option, isPaddingOption)
	}
	if !p.enabled {
		return out
	}
	trailer := out.IsTsig()
	trailerSize := 0
	if trailer != nil {
		var ok bool
		trailerSize, ok = paddedTSIGSize(trailer)
		if !ok {
			return out
		}
		// miekg signs the packed message without TSIG, then appends the
		// uncompressed trailer. Measure the same representation.
		out.Extra = out.Extra[:len(out.Extra)-1]
		defer func() { out.Extra = append(out.Extra, trailer) }()
	}
	createdOPT := opt == nil
	if createdOPT {
		out.SetEdns0(uint16(p.limit), p.do) //nolint:gosec // Captured uint16 payload size, with a 512-byte minimum.
		opt = out.IsEdns0()
	}
	pad := &dns.EDNS0_PADDING{}
	opt.Option = append(opt.Option, pad)
	wire, err := out.Pack()
	base := len(wire) + trailerSize
	target := (base + responsePaddingBlock - 1) / responsePaddingBlock * responsePaddingBlock
	// When the next complete block does not fit, use the remaining peer
	// budget. RFC 7830 still requires padding whenever its option fits.
	target = min(target, p.limit, maxDNSMessage)
	if err != nil || base > target {
		// Preserve the answer and its other EDNS options; never truncate
		// records or exceed the peer's payload size solely to add padding.
		opt.Option = opt.Option[:len(opt.Option)-1]
		if createdOPT {
			return m
		}
		return out
	}
	pad.Padding = make([]byte, target-base)
	return out
}

func isPaddingOption(option dns.EDNS0) bool { return option.Option() == dns.EDNS0PADDING }

func paddedTSIGSize(t *dns.TSIG) (int, bool) {
	size := dns.Len(t)
	if t.Error == dns.RcodeBadKey || t.Error == dns.RcodeBadSig {
		return size, true // these errors carry no MAC
	}
	switch dns.CanonicalName(t.Algorithm) {
	case dns.HmacSHA256:
		return size + sha256.Size, true
	case dns.HmacSHA512:
		return size + sha512.Size, true
	default:
		return 0, false // no guessed signature length for an unknown signer
	}
}
