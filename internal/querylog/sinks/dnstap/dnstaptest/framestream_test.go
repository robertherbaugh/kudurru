package dnstaptest

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
)

// The handshake and a data frame as the Frame Streams description lays
// them out, written by hand: escape, control length, control type, then
// CONTENT_TYPE (1), its length (22) and "protobuf:dnstap.Dnstap".
const (
	goldenReady  = "00000000" + "00000022" + "00000004" + "00000001" + "00000016" + "70726f746f6275663a646e737461702e446e73746170"
	goldenAccept = "00000000" + "00000022" + "00000001" + "00000001" + "00000016" + "70726f746f6275663a646e737461702e446e73746170"
	goldenStart  = "00000000" + "00000022" + "00000002" + "00000001" + "00000016" + "70726f746f6275663a646e737461702e446e73746170"
	goldenStop   = "00000000" + "00000004" + "00000003"
	goldenFinish = "00000000" + "00000004" + "00000005"
	goldenData   = "00000003" + "0a0102"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFrameReaderGolden reads the hand-written stream with the collector's
// reader and requires the sink's framing to produce the same bytes, so
// both sides are pinned to the description rather than to each other.
func TestFrameReaderGolden(t *testing.T) {
	stream := unhex(t, goldenReady+goldenStart+goldenData+goldenStop)
	r := NewFrameReader(bytes.NewReader(stream), 0)
	for _, want := range []uint32{fstrmReady, fstrmStart} {
		c, err := r.control(want)
		if err != nil || len(c.ContentTypes) != 1 || c.ContentTypes[0] != fstrmContentType {
			t.Fatalf("control %d: %+v %v", want, c, err)
		}
	}
	data, c, err := r.Next()
	if err != nil || c != nil || !bytes.Equal(data, []byte{0x0a, 1, 2}) {
		t.Fatalf("data frame: %x %+v %v", data, c, err)
	}
	if c, err := r.control(fstrmStop); err != nil || len(c.ContentTypes) != 0 {
		t.Fatalf("STOP: %+v %v", c, err)
	}
	if _, _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream: %v", err)
	}
	for name, tc := range map[string]struct{ got, want []byte }{
		"READY":  {dnstap.AppendControl(nil, dnstap.ControlReady, dnstap.ContentType), unhex(t, goldenReady)},
		"START":  {dnstap.AppendControl(nil, dnstap.ControlStart, dnstap.ContentType), unhex(t, goldenStart)},
		"STOP":   {dnstap.AppendControl(nil, dnstap.ControlStop), unhex(t, goldenStop)},
		"data":   {dnstap.AppendData(nil, []byte{0x0a, 1, 2}), unhex(t, goldenData)},
		"ACCEPT": {controlFrame(fstrmAccept, fstrmContentType), unhex(t, goldenAccept)},
		"FINISH": {controlFrame(fstrmFinish), unhex(t, goldenFinish)},
	} {
		if !bytes.Equal(tc.got, tc.want) {
			t.Errorf("%s: %x, want %x", name, tc.got, tc.want)
		}
	}
}

// TestFrameReaderRefuses covers what the description rules out.
func TestFrameReaderRefuses(t *testing.T) {
	ct := hex.EncodeToString([]byte(fstrmContentType))
	for name, tc := range map[string]struct {
		stream string
		want   error
	}{
		"control type 0":          {"00000000" + "00000004" + "00000000", ErrFrame},
		"control type 6":          {"00000000" + "00000004" + "00000006", ErrFrame},
		"control frame too short": {"00000000" + "00000002" + "0000", ErrFrame},
		"control frame too long":  {"00000000" + "00000201", ErrFrame},
		"unknown field type":      {"00000000" + "00000022" + "00000004" + "00000002" + "00000016" + ct, ErrFrame},
		"field beyond the frame":  {"00000000" + "00000022" + "00000004" + "00000001" + "00000017" + ct, ErrFrame},
		"truncated field header":  {"00000000" + "00000008" + "00000004" + "00000001", ErrFrame},
		"content type too long":   {"00000000" + "0000010d" + "00000004" + "00000001" + "00000101" + strings.Repeat("61", 257), ErrFrame},
		"START with two types":    {"00000000" + "00000032" + "00000002" + "00000001" + "00000016" + ct + "00000001" + "00000008" + "6161616161616161", ErrFrame},
		"STOP with a field":       {"00000000" + "00000022" + "00000003" + "00000001" + "00000016" + ct, ErrFrame},
		"data frame over the cap": {"00000011", ErrFrame},
		"data frame cut short":    {"00000004" + "0a01", io.ErrUnexpectedEOF},
		"escape without a length": {"00000000", io.ErrUnexpectedEOF},
		"control frame cut short": {"00000000" + "00000022" + "00000004", io.ErrUnexpectedEOF},
	} {
		r := NewFrameReader(bytes.NewReader(unhex(t, tc.stream)), 16)
		if _, _, err := r.Next(); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}
