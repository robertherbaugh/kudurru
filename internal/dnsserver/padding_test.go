package dnsserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go/http3"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func paddedQuery(size uint16) *dns.Msg {
	q := query("example.com")
	q.Id = 0
	q.SetEdns0(size, true)
	q.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_PADDING{Padding: make([]byte, 12)}}
	return q
}

func TestResponsePaddingBoundariesAndDetachedAnswers(t *testing.T) {
	for _, size := range []uint16{0, 512, 600, 1232, 65535} {
		for _, n := range []int{0, 300, 400, 450, 460, 480, 550, 1000} {
			q := paddedQuery(size)
			original := new(dns.Msg)
			original.SetReply(q)
			original.Compress = true
			original.AuthenticatedData = true
			original.SetEdns0(size, true)
			original.IsEdns0().Option = []dns.EDNS0{
				&dns.EDNS0_PADDING{Padding: []byte{1, 2, 3}},
				&dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeOther, ExtraText: strings.Repeat("x", n)},
			}
			before, _ := original.Pack()
			p := paddingFor(q, pipeline.ProtoDoT)
			got := p.apply(original)
			wire, err := got.Pack()
			if err != nil {
				t.Fatal("padded response cannot pack")
			}
			after, _ := original.Pack()
			if got == original || !bytes.Equal(before, after) || !got.AuthenticatedData || !got.IsEdns0().Do() {
				t.Fatal("padding mutated the original answer or DNSSEC flags")
			}
			options := got.IsEdns0().Option
			if len(options) == 2 {
				pad, ok := options[1].(*dns.EDNS0_PADDING)
				if !ok || !bytes.Equal(pad.Padding, make([]byte, len(pad.Padding))) || len(wire) > max(512, int(size)) || len(wire)%468 != 0 && len(wire) != max(512, int(size)) {
					t.Fatal("padding was not last, zero-filled, block aligned or peer bounded")
				}
			} else if len(options) != 1 || len(wire)+4 <= max(512, int(size)) {
				t.Fatal("padding omitted even though its option fits")
			}
			if ede, ok := options[0].(*dns.EDNS0_EDE); !ok || len(ede.ExtraText) != n {
				t.Fatal("padding changed another EDNS option")
			}
			// The same cached answer can serve a plaintext or unpadded peer.
			for _, proto := range []pipeline.Proto{pipeline.ProtoUDP, pipeline.ProtoTCP} {
				plain := paddingFor(q, proto).apply(got)
				if slices.ContainsFunc(plain.IsEdns0().Option, isPaddingOption) {
					t.Fatal("encrypted response padding leaked onto plaintext transport")
				}
			}
		}
	}
}

func TestResponsePaddingDoesNotGrowAnswersWithoutOptionSpace(t *testing.T) {
	for _, limit := range []int{512, 65535} {
		q := paddedQuery(uint16(limit)) //nolint:gosec // Fixed valid DNS payload sizes.
		m := new(dns.Msg)
		m.SetReply(q)
		rr := &dns.RFC3597{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: 65280, Class: dns.ClassINET}}
		m.Answer = []dns.RR{rr}
		base, err := m.Pack()
		if err != nil {
			t.Fatal("pack boundary fixture")
		}
		rr.Rdata = strings.Repeat("00", limit-len(base))
		before, err := m.Pack()
		if err != nil || len(before) != limit {
			t.Fatal("boundary fixture has wrong size")
		}
		got := paddingFor(q, pipeline.ProtoDoT).apply(m)
		after, err := got.Pack()
		if err != nil || !bytes.Equal(before, after) || got.IsEdns0() != nil {
			t.Fatal("padding added an OPT or changed records without room")
		}
	}
}

func TestResponsePaddingTransportNegotiation(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: allSpecs(), stages: []pipeline.Stage{localAnswer{}}})
	for _, proto := range Protocols() {
		t.Run(string(proto), func(t *testing.T) {
			for _, offer := range []string{"padding", "edns only", "no edns"} {
				q := paddedQuery(1232)
				switch offer {
				case "edns only":
					q.IsEdns0().Option = nil
				case "no edns":
					q.Extra = nil
				}
				wire := exchangePaddingWire(t, f, proto, q)
				m := new(dns.Msg)
				if err := m.Unpack(wire); err != nil {
					t.Fatal("invalid response")
				}
				checkA(t, m)
				opt := m.IsEdns0()
				want := offer == "padding" && proto.TLS()
				present := opt != nil && slices.ContainsFunc(opt.Option, isPaddingOption)
				if present != want || want && (len(wire) != 468 || !opt.Do()) {
					t.Fatalf("transport %s offer %s: padding=%t wire size=%d", proto, offer, present, len(wire))
				}
			}
		})
	}
}

func exchangePaddingWire(t *testing.T, f *fixture, proto Protocol, q *dns.Msg) []byte {
	t.Helper()
	wire, err := q.Pack()
	if err != nil {
		t.Fatal("pack request")
	}
	switch proto {
	case ProtoDoH, ProtoDoH3:
		client := dohClient(t, f, true)
		if proto == ProtoDoH3 {
			tr := &http3.Transport{TLSClientConfig: (&tlsPool{f.cert}).client()}
			defer func() { _ = tr.Close() }()
			client = &http.Client{Transport: tr, Timeout: 3 * time.Second}
		}
		res, err := client.Get("https://" + f.addr(proto) + DefaultDoHPath + "?dns=" + base64.RawURLEncoding.EncodeToString(wire))
		if err != nil {
			t.Fatal("HTTPS exchange failed")
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != http.StatusOK || int64(len(body)) != res.ContentLength {
			t.Fatal("HTTPS response framing failed")
		}
		return body
	case ProtoDoQ:
		conn, err := doqDial(t, f.addr(proto), &tlsPool{f.cert})
		if err != nil {
			t.Fatal("QUIC dial failed")
		}
		defer closeQUIC(conn)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal("QUIC stream failed")
		}
		_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := stream.Write(append(lenPrefix(wire), wire...)); err != nil {
			t.Fatal("QUIC write failed")
		}
		_ = stream.Close()
		body, err := io.ReadAll(stream)
		if err != nil || len(body) < 2 || int(binary.BigEndian.Uint16(body[:2])) != len(body)-2 {
			t.Fatal("QUIC response framing failed")
		}
		return body[2:]
	default:
		var conn net.Conn
		if proto == ProtoDoT {
			conn, err = tls.Dial("tcp", f.addr(proto), (&tlsPool{f.cert}).client("dot"))
		} else {
			conn, err = net.DialTimeout(proto.Network(), f.addr(proto), 3*time.Second)
		}
		if err != nil {
			t.Fatal("DNS dial failed")
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		c := &dns.Conn{Conn: conn, UDPSize: 65535}
		if _, err := c.Write(wire); err != nil {
			t.Fatal("DNS write failed")
		}
		body, err := c.ReadMsgHeader(nil)
		if err != nil {
			t.Fatal("DNS response read failed")
		}
		return body
	}
}

func TestResponsePaddingDoTFinalTSIGLength(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoT}}, tsig: testKeyring(t), stages: []pipeline.Stage{localAnswer{}}})
	for _, key := range []struct {
		name, algorithm string
		secret          []byte
	}{{xfrKey, dns.HmacSHA256, xfrSecret}, {xfrKey512, dns.HmacSHA512, xfrSecret512}} {
		q := paddedQuery(1232)
		q.SetTsig(key.name, key.algorithm, 300, time.Now().Unix())
		wire, mac, err := dns.TsigGenerate(q, b64(key.secret), "", false)
		if err != nil {
			t.Fatal("sign fixture request")
		}
		conn, err := tls.Dial("tcp", f.addr(ProtoDoT), (&tlsPool{f.cert}).client("dot"))
		if err != nil {
			t.Fatal("TLS dial failed")
		}
		c := &dns.Conn{Conn: conn}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = c.Write(wire)
		if err != nil {
			t.Fatal("signed request write failed")
		}
		answer, err := c.ReadMsgHeader(nil)
		_ = c.Close()
		if err != nil || len(answer) != 468 || dns.TsigVerify(bytes.Clone(answer), b64(key.secret), mac, false) != nil {
			t.Fatal("padding changed the final signed response length or validity")
		}
	}
}

type paddingTransferStage struct{}

func (paddingTransferStage) Name() string { return "local" }

func (paddingTransferStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	soa := &dns.SOA{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeSOA, Class: dns.ClassINET}, Ns: "ns.example.com.", Mbox: "hostmaster.example.com.", Serial: 1}
	first := new(dns.Msg)
	first.SetReply(req.Msg)
	first.Compress = true
	first.Answer = []dns.RR{soa}
	return &pipeline.Response{Msg: first, Transfer: func(yield func(*dns.Msg) bool) {
		if !yield(first) {
			return
		}
		middle := first.Copy()
		middle.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "ns.example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 1)}}
		if !yield(middle) {
			return
		}
		yield(first.Copy())
	}}, nil
}

func TestResponsePaddingSignedDoTTransfer(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoT}}, tsig: testKeyring(t), stages: []pipeline.Stage{paddingTransferStage{}}})
	q := paddedQuery(1232)
	q.Question[0].Qtype = dns.TypeAXFR
	q.SetTsig(xfrKey, dns.HmacSHA256, 300, time.Now().Unix())
	wire, mac, err := dns.TsigGenerate(q, b64(xfrSecret), "", false)
	if err != nil {
		t.Fatal("sign transfer request")
	}
	conn, err := tls.Dial("tcp", f.addr(ProtoDoT), (&tlsPool{f.cert}).client("dot"))
	if err != nil {
		t.Fatal("TLS transfer dial failed")
	}
	defer func() { _ = conn.Close() }()
	c := &dns.Conn{Conn: conn}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(wire); err != nil {
		t.Fatal("transfer request write failed")
	}
	for i, wantType := range []uint16{dns.TypeSOA, dns.TypeA, dns.TypeSOA} {
		answer, err := c.ReadMsgHeader(nil)
		// The verifier rewrites ARCOUNT in its input while removing TSIG.
		// Preserve the original received bytes for record/chain assertions.
		if err != nil || len(answer) != 468 || dns.TsigVerify(bytes.Clone(answer), b64(xfrSecret), mac, i > 0) != nil {
			t.Fatal("padded transfer broke its wire boundary or TSIG chain")
		}
		m := new(dns.Msg)
		if m.Unpack(answer) != nil || len(m.Answer) != 1 || m.Answer[0].Header().Rrtype != wantType || m.IsTsig() == nil {
			t.Fatal("padded transfer changed its records or trailing signature")
		}
		mac = m.IsTsig().MAC
	}
}

func TestResponsePaddingDoTTSIGRefusal(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoT}}, tsig: testKeyring(t)})
	q := paddedQuery(1232)
	q.SetTsig(xfrKey, dns.HmacSHA256, 300, time.Now().Unix())
	wire, _, err := dns.TsigGenerate(q, b64(otherSecret), "", false)
	if err != nil {
		t.Fatal("sign invalid fixture")
	}
	conn, err := tls.Dial("tcp", f.addr(ProtoDoT), (&tlsPool{f.cert}).client("dot"))
	if err != nil {
		t.Fatal("TLS dial failed")
	}
	defer func() { _ = conn.Close() }()
	c := &dns.Conn{Conn: conn}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(wire); err != nil {
		t.Fatal("write invalid request")
	}
	answer, err := c.ReadMsgHeader(nil)
	m := new(dns.Msg)
	if err != nil || len(answer) != 468 || m.Unpack(answer) != nil || m.Rcode != dns.RcodeNotAuth || m.IsTsig() == nil || m.IsTsig().Error != dns.RcodeBadSig || m.IsTsig().MAC != "" || f.rec.count() != 0 {
		t.Fatal("padding changed TSIG refusal or allowed pipeline execution")
	}
}
