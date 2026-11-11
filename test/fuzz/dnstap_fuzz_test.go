package fuzz

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap/dnstaptest"
)

func dnstapSink(f *testing.F, raw bool) *dnstap.Sink {
	f.Helper()
	settings := config.QuerylogDnstap{Target: "unix:/run/fuzz.sock", Raw: raw, Types: config.DnstapTypes}
	cfg, err := config.Load("", nil)
	if err != nil {
		f.Fatal(err)
	}
	cfg.Querylog.Sinks = []config.QuerylogSink{{Name: "fuzz", Type: config.QuerylogSinkDnstap, Dnstap: &settings}}
	specs, err := querylog.SinkSpecsFrom(cfg.Querylog)
	if err != nil {
		f.Fatal(err)
	}
	sink, err := dnstap.New(specs[0], dnstap.Options{Node: "fuzz", Version: "kudurru/fuzz", Audit: func(context.Context, dnstap.RawStart) error { return nil }})
	if err != nil {
		f.Fatal(err)
	}
	return sink
}

// FuzzDnstapEncode encodes query events built from arbitrary fields with
// the dnstap sink's own encoder (raw and not raw), reads the stream back
// with the Frame Streams reader and decodes every frame with the strict
// dnstap parser of the tests. Every frame must decode, and a sink that is
// not raw must never send the wire messages an event carries (D372).
func FuzzDnstapEncode(f *testing.F) {
	sinks := map[bool]*dnstap.Sink{false: dnstapSink(f, false), true: dnstapSink(f, true)}
	f.Add("www.example.com.", []byte{192, 0, 2, 1}, uint16(1), uint16(1), 0, uint8(1), "default/quad9", "laptop", "default/ads", []byte("query-wire"), []byte("response-wire"), uint16(5353), int64(1_762_000_000_000_000_000), false)
	f.Add("hidden", []byte{}, uint16(28), uint16(1), 3, uint8(4), "", "anon-0123456789ab", "", []byte{}, []byte{}, uint16(0), int64(0), true)
	f.Add("bad..name\x00", []byte{0x20, 1, 0xd, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, uint16(65535), uint16(255), 4095, uint8(255), "x y", "a=\"b\"", "\n", []byte{0}, []byte{1}, uint16(65535), int64(-1), false)
	f.Fuzz(func(t *testing.T, name string, addr []byte, qtype, qclass uint16, rcode int, status uint8, upstream, clientName, list string, query, response []byte, port uint16, unixNano int64, cached bool) {
		client, _ := netip.AddrFromSlice(addr)
		ev := &querylog.Event{
			Time: time.Unix(0, unixNano), Latency: time.Millisecond, Proto: pipeline.Proto(status % 6), Client: client, Name: name, QType: qtype, QClass: qclass,
			Status: pipeline.Status(status), Rcode: rcode & 0xFFF, Upstream: upstream, ClientName: clientName, List: list, Zone: name,
			ReplyType: querylog.ReplyType(status % 12), Flags: querylog.Flags{Cached: cached, RD: status&1 == 1, DO: status&2 == 2, Prefetch: status&4 == 4},
			Wire: &querylog.Wire{Query: query, Response: response, ClientPort: port},
		}
		for raw, sink := range sinks {
			stream := sink.AppendEvent(nil, ev)
			reader := dnstap.NewReader(bytes.NewReader(stream))
			for {
				data, control, err := reader.Next(nil)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil || control != nil {
					t.Fatalf("raw=%v: the encoder wrote an unreadable stream: %v %v", raw, err, control)
				}
				frame, err := dnstaptest.Decode(data)
				if err != nil {
					t.Fatalf("raw=%v: a frame does not decode: %v", raw, err)
				}
				if frame.Identity != "fuzz" || frame.Version != "kudurru/fuzz" {
					t.Fatalf("identity %q version %q", frame.Identity, frame.Version)
				}
				m := frame.Message
				if !raw && ((len(query) > 0 && bytes.Equal(m.QueryMessage, query)) || (len(response) > 0 && bytes.Equal(m.ResponseMessage, response)) || m.QueryPort != 0) {
					t.Fatalf("a sink that is not raw sent the wire messages or the port: %+v", m)
				}
			}
		}
	})
}

// FuzzFrameStreamRead reads arbitrary bytes as a Frame Streams stream and
// every data frame as a dnstap message: no panic, no frame beyond the
// bounds, and control frames that parse re-encode to the same bytes.
func FuzzFrameStreamRead(f *testing.F) {
	f.Add(dnstap.AppendControl(nil, dnstap.ControlReady, dnstap.ContentType))
	f.Add(dnstap.AppendData(dnstap.AppendControl(nil, dnstap.ControlStart, dnstap.ContentType), []byte{0x78, 1}))
	f.Add([]byte{0, 0, 0, 0, 0, 0, 2, 1})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, stream []byte) {
		reader := dnstap.NewReader(bytes.NewReader(stream))
		reader.MaxData = 4096
		for {
			data, control, err := reader.Next(nil)
			if err != nil {
				return
			}
			if control != nil {
				again, err := dnstap.ParseControl(dnstap.AppendControl(nil, control.Type, control.ContentTypes...)[8:])
				if err != nil || again.Type != control.Type || len(again.ContentTypes) != len(control.ContentTypes) {
					t.Fatalf("control frame %+v does not round-trip: %+v %v", control, again, err)
				}
				continue
			}
			if len(data) == 0 || len(data) > 4096 {
				t.Fatalf("a data frame of %d bytes", len(data))
			}
			_, _ = dnstaptest.Decode(data)
		}
	})
}
