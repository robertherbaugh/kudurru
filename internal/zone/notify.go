package zone

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

// DefaultNotifyTimeout bounds one NOTIFY exchange.
const DefaultNotifyTimeout = 3 * time.Second

// Notify is one outbound NOTIFY message (RFC 1996): "zone changed, its SOA
// serial is now Serial", sent to one secondary.
type Notify struct {
	// Zone is the zone name (absolute).
	Zone string
	// Serial is the new SOA serial, carried in the answer section as RFC
	// 1996 §3.7 allows so the secondary can skip the SOA query when it
	// already has it.
	Serial uint32
	// SOA, when set, is sent in the answer section instead of a synthesised
	// one carrying only Serial.
	SOA *dns.SOA
	// Target is the secondary's address.
	Target netip.AddrPort
	// Source, when valid, is the local address the message is sent from
	// (secondaries match NOTIFY against their primaries/allow-notify list,
	// §4.6). Its port is ignored.
	Source netip.Addr
	// Timeout bounds the exchange; DefaultNotifyTimeout when zero.
	Timeout time.Duration
	// TSIG signs the message when KeyName is set (RFC 8945); the reply
	// must then verify with the same key.
	KeyName   string
	Algorithm string
	TSIG      dns.TsigProvider
}

// NotifyError classifies a failed NOTIFY exchange.
type NotifyError struct {
	// Class is "timeout", "network", "rcode" or "tsig"; it is the result
	// label of kudurru_zone_notify_total.
	Class string
	// Rcode is the reply's rcode for Class "rcode".
	Rcode int
	Err   error
}

func (e *NotifyError) Error() string {
	if e.Class == "rcode" {
		return "notify: secondary answered " + dns.RcodeToString[e.Rcode]
	}
	return "notify: " + e.Class + ": " + e.Err.Error()
}

func (e *NotifyError) Unwrap() error { return e.Err }

// NotifyMessage returns the NOTIFY query for n: opcode NOTIFY, AA set, the
// zone's SOA as the question (RFC 1996 §3.3) and the SOA with the new
// serial in the answer section.
func NotifyMessage(n Notify) *dns.Msg {
	name := dns.Fqdn(n.Zone)
	m := new(dns.Msg)
	m.SetNotify(name)
	m.Authoritative = true
	soa := n.SOA
	if soa == nil {
		soa = &dns.SOA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeSOA, Class: dns.ClassINET}, Ns: ".", Mbox: ".", Serial: n.Serial}
	} else {
		soa = dns.Copy(soa).(*dns.SOA)
		soa.Serial = n.Serial
	}
	m.Answer = []dns.RR{soa}
	return m
}

// SendNotify sends one NOTIFY over UDP and waits for the matching reply
// (RFC 1996 §3.6, §4.7). Retries are the caller's job. A reply with
// NOERROR is success; any other rcode, a TSIG failure, a timeout or a
// network error is returned as a *NotifyError.
func SendNotify(ctx context.Context, n Notify) error {
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = DefaultNotifyTimeout
	}
	c := &dns.Client{Net: "udp", Timeout: timeout}
	if n.Source.IsValid() {
		c.Dialer = &net.Dialer{Timeout: timeout, LocalAddr: &net.UDPAddr{IP: n.Source.AsSlice(), Zone: n.Source.Zone()}}
	}
	m := NotifyMessage(n)
	if n.KeyName != "" {
		if n.TSIG == nil {
			return &NotifyError{Class: "tsig", Err: errors.New("no TSIG provider for key " + n.KeyName)}
		}
		c.TsigProvider = n.TSIG
		m.SetTsig(KeyName(n.KeyName), dns.CanonicalName(n.Algorithm), 300, time.Now().Unix())
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, _, err := c.ExchangeContext(ctx, m, n.Target.String())
	if err != nil {
		return classifyNotify(err)
	}
	if n.KeyName != "" && r.IsTsig() == nil {
		// RFC 8945 §5.3: a signed request gets a signed reply.
		return &NotifyError{Class: "tsig", Err: errors.New("the reply to a signed NOTIFY is not signed")}
	}
	if r.Rcode != dns.RcodeSuccess {
		return &NotifyError{Class: "rcode", Rcode: r.Rcode, Err: fmt.Errorf("rcode %s", dns.RcodeToString[r.Rcode])}
	}
	if r.Opcode != dns.OpcodeNotify {
		return &NotifyError{Class: "network", Err: fmt.Errorf("reply has opcode %s", dns.OpcodeToString[r.Opcode])}
	}
	return nil
}

func classifyNotify(err error) *NotifyError {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return &NotifyError{Class: "timeout", Err: err}
	case errors.Is(err, dns.ErrSig), errors.Is(err, dns.ErrSecret), errors.Is(err, dns.ErrKeyAlg),
		errors.Is(err, dns.ErrTime), errors.Is(err, dns.ErrNoSig), errors.Is(err, dns.ErrAuth):
		return &NotifyError{Class: "tsig", Err: err}
	}
	return &NotifyError{Class: "network", Err: err}
}
