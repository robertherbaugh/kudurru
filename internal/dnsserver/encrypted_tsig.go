package dnsserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// encryptedSigner is a transport-local capability. It is never exposed to
// pipeline stages and cannot authorize UPDATE or NOTIFY. The original parsed
// message, immutable installed provider, request MAC and signature deadline all
// precede fresh-ID adaptation. None can be repaired with a later key lookup.
type encryptedSigner struct {
	request              *dns.Msg
	provider             dns.TsigProvider
	verifiedAt, deadline time.Time
}

// encryptedReply couples the serialized reply with its HTTP freshness policy;
// unsigned HTTP responses do not need another DNS decode to recover their TTL.
type encryptedReply struct {
	wire  []byte
	cache string
}

func packEncryptedReply(m *dns.Msg) (*encryptedReply, error) {
	wire, err := m.Pack()
	return &encryptedReply{wire: wire, cache: cacheControl(m)}, err
}

// resolveEncrypted verifies the original bounded wire before resolving a
// detached unsigned working message. Only the authenticated key name crosses
// the pipeline seam; native mutation proof issuance is deliberately untouched.
func (h *Handler) resolveEncrypted(ctx context.Context, e *env, wire []byte, req *dns.Msg, client pipeline.Client) (*encryptedReply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	padding := paddingFor(req, client.Proto)
	if !encryptedTSIGFrame(req, wire) {
		m := new(dns.Msg).SetRcode(req, dns.RcodeFormatError)
		return packEncryptedReply(padding.apply(m))
	}
	var sig *encryptedSigner
	if ts := req.IsTsig(); ts != nil {
		provider := TSIGProvider(nil)
		if e != nil {
			if p, ok := e.tsig.(keyringProvider); ok {
				// A Lookup-only provider can rotate during verification and has
				// no installed generation to pin. Fail closed on encrypted paths.
				if keys, immutable := p.k.(*StaticKeyring); immutable && keys != nil {
					provider = p
				}
			}
		}
		now := time.Now()
		sig = &encryptedSigner{request: req.Copy(), provider: provider, verifiedAt: now}
		// The verifier rewrites header fields while stripping TSIG. Give it a
		// private copy of the exact received encoding, never a repacked message.
		err := dns.TsigVerifyWithProvider(bytes.Clone(wire), provider, "", false)
		if err == nil {
			if ts.TimeSigned > math.MaxInt64-uint64(ts.Fudge) {
				err = dns.ErrTime
			} else {
				sig.deadline = time.Unix(int64(ts.TimeSigned)+int64(ts.Fudge), 0) //nolint:gosec // bounded above before conversion
				if actual := time.Now(); actual.Before(now) || !actual.Before(sig.deadline) {
					err = dns.ErrTime
				}
			}
		}
		if err != nil {
			reason, code := encryptedTSIGFailure(err)
			if e != nil && e.onTSIGFailure != nil {
				e.onTSIGFailure(reason)
			}
			return sig.failure(code, padding)
		}
		client.TSIGKey = TSIGKeyName(ts.Hdr.Name)
	}
	working := req.Copy()
	if sig != nil {
		working.Extra = working.Extra[:len(working.Extra)-1]
	}
	resp := h.resolveFreshID(ctx, working, client)
	if resp == nil || ctx.Err() != nil {
		return nil, nil
	}
	if sig != nil {
		actual := time.Now()
		if actual.Before(sig.verifiedAt) || !actual.Before(sig.deadline) {
			if e != nil && e.onTSIGFailure != nil {
				e.onTSIGFailure(TSIGBadTime)
			}
			return sig.failure(dns.RcodeBadTime, padding)
		}
	}
	encode := func(m *dns.Msg) (*encryptedReply, error) {
		// Responses may share cache backing. Signing and padding never alter
		// that backing, and an upstream's TSIG cannot become this hop's proof.
		m = m.Copy()
		for i := len(m.Extra) - 1; i >= 0; i-- {
			if m.Extra[i].Header().Rrtype == dns.TypeTSIG {
				m.Extra = append(m.Extra[:i], m.Extra[i+1:]...)
			}
		}
		if sig == nil {
			return packEncryptedReply(padding.apply(m))
		}
		t := sig.request.IsTsig()
		m.SetTsig(t.Hdr.Name, t.Algorithm, tsigFudge, time.Now().Unix())
		m = padding.apply(m)
		out, _, err := dns.TsigGenerateWithProvider(m, sig.provider, t.MAC, false)
		return &encryptedReply{wire: out, cache: "no-store"}, err
	}
	out, err := encode(resp)
	if err != nil {
		out, err = encode(servfail(req))
	}
	// Packing/signing itself cannot extend the original request's lifetime.
	if sig != nil {
		actual := time.Now()
		if actual.Before(sig.verifiedAt) || !actual.Before(sig.deadline) {
			if e != nil && e.onTSIGFailure != nil {
				e.onTSIGFailure(TSIGBadTime)
			}
			return sig.failure(dns.RcodeBadTime, padding)
		}
	}
	if ctx.Err() != nil {
		return nil, nil
	}
	return out, err
}

func encryptedTSIGFailure(err error) (string, uint16) {
	switch {
	case errors.Is(err, dns.ErrSecret), errors.Is(err, dns.ErrKeyAlg):
		return TSIGBadKey, dns.RcodeBadKey
	case errors.Is(err, dns.ErrTime):
		return TSIGBadTime, dns.RcodeBadTime
	default:
		return TSIGBadSig, dns.RcodeBadSig
	}
}

func (s *encryptedSigner) failure(code uint16, padding responsePadding) (*encryptedReply, error) {
	r := s.request
	t := r.IsTsig()
	m := new(dns.Msg).SetRcode(r, dns.RcodeNotAuth)
	rr := &dns.TSIG{Hdr: dns.RR_Header{Name: t.Hdr.Name, Rrtype: dns.TypeTSIG, Class: dns.ClassANY}, Algorithm: t.Algorithm, Fudge: t.Fudge, TimeSigned: t.TimeSigned, OrigId: r.Id, Error: code}
	if code == dns.RcodeBadTime {
		var clock [8]byte
		binary.BigEndian.PutUint64(clock[:], uint64(time.Now().Unix())) //nolint:gosec // positive Unix time
		rr.OtherLen, rr.OtherData = 6, hex.EncodeToString(clock[2:])
	}
	m.Extra = append(m.Extra, rr)
	m = padding.apply(m)
	out, _, err := dns.TsigGenerateWithProvider(m, s.provider, t.MAC, false)
	return &encryptedReply{wire: out, cache: "no-store"}, err
}

// TSIG must occur exactly once, in the last additional RR, and at the end of
// the actual wire. A nonfinal TSIG cannot silently turn into an unsigned query.
// The extra decoding is bounded by the existing 65535-byte transport limits.
func encryptedTSIGFrame(req *dns.Msg, wire []byte) bool {
	count := 0
	for _, section := range [][]dns.RR{req.Answer, req.Ns, req.Extra} {
		for _, rr := range section {
			if rr.Header().Rrtype == dns.TypeTSIG {
				count++
			}
		}
	}
	if count == 0 {
		return true
	}
	t := req.IsTsig()
	if count != 1 || t == nil || t.Hdr.Class != dns.ClassANY || t.Hdr.Ttl != 0 || t.Error != 0 || t.MAC == "" {
		return false
	}
	if len(wire) < 12 || len(wire) > maxDNSMessage {
		return false
	}
	offset := 12
	for range req.Question {
		_, next, err := dns.UnpackDomainName(wire, offset)
		if err != nil || next+4 > len(wire) {
			return false
		}
		offset = next + 4
	}
	for range len(req.Answer) + len(req.Ns) + len(req.Extra) {
		_, next, err := dns.UnpackRR(wire, offset)
		if err != nil || next <= offset {
			return false
		}
		offset = next
	}
	return offset == len(wire)
}
