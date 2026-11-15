package dnstaptest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A second Frame Streams implementation, written for the collector from the
// fstrm protocol description (https://farsightsec.github.io/fstrm/) and
// deliberately independent of the sink's framestream.go: it imports nothing
// from the sink, names its own constants and is stricter than the sink's
// reader. The sink's framing is therefore checked against the description,
// not against itself.
//
// The stream, as the description defines it: every frame starts with a
// 32-bit big-endian length. A non-zero length is a data frame of that many
// bytes. A zero length is the escape of a control frame: a 32-bit length of
// the control frame (at most 512 bytes), then a 32-bit control type and its
// fields, each a 32-bit field type, a 32-bit length and that many bytes.
// The only field type is CONTENT_TYPE (1), at most 256 bytes. The control
// types are ACCEPT (1), START (2), STOP (3), READY (4) and FINISH (5); START
// carries at most one content type, STOP and FINISH carry none.

// dnstap's content type.
const fstrmContentType = "protobuf:dnstap.Dnstap"

// Control types and the field type of the description.
const (
	fstrmAccept uint32 = 1
	fstrmStart  uint32 = 2
	fstrmStop   uint32 = 3
	fstrmReady  uint32 = 4
	fstrmFinish uint32 = 5

	fstrmFieldContentType uint32 = 1
)

// Bounds of the description, and of a data frame this collector accepts.
const (
	fstrmMaxControl     = 512
	fstrmMaxContentType = 256
	fstrmMaxData        = 1 << 20
)

// ErrFrame is a stream that breaks the Frame Streams description.
var ErrFrame = errors.New("dnstaptest: frame streams violation")

// Control is a control frame read by FrameReader.
type Control struct {
	Type         uint32
	ContentTypes []string
}

// FrameReader reads one Frame Streams stream strictly.
type FrameReader struct {
	r       *bufio.Reader
	maxData uint32
}

// NewFrameReader reads frames from r and refuses a data frame longer than
// maxData bytes (zero means 1 MiB).
func NewFrameReader(r io.Reader, maxData int) *FrameReader {
	limit := uint32(fstrmMaxData)
	if maxData > 0 && maxData < fstrmMaxData {
		limit = uint32(maxData) //nolint:gosec // G115: bounded above by fstrmMaxData
	}
	return &FrameReader{r: bufio.NewReader(r), maxData: limit}
}

// Next returns the next frame: a data frame (control nil) or a control
// frame. A clean end of the stream before a frame starts is io.EOF; an end
// inside a frame is io.ErrUnexpectedEOF.
func (fr *FrameReader) Next() (data []byte, control *Control, err error) {
	length, err := fr.word()
	if err != nil {
		return nil, nil, err
	}
	if length != 0 {
		if length > fr.maxData {
			return nil, nil, fmt.Errorf("%w: data frame of %d bytes", ErrFrame, length)
		}
		data = make([]byte, length)
		if _, err := io.ReadFull(fr.r, data); err != nil {
			return nil, nil, unexpected(err)
		}
		return data, nil, nil
	}
	length, err = fr.word()
	if err != nil {
		return nil, nil, unexpected(err)
	}
	if length < 4 || length > fstrmMaxControl {
		return nil, nil, fmt.Errorf("%w: control frame of %d bytes", ErrFrame, length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(fr.r, body); err != nil {
		return nil, nil, unexpected(err)
	}
	c := &Control{Type: binary.BigEndian.Uint32(body[:4])}
	if c.Type < fstrmAccept || c.Type > fstrmFinish {
		return nil, nil, fmt.Errorf("%w: control type %d", ErrFrame, c.Type)
	}
	for rest := body[4:]; len(rest) > 0; {
		if len(rest) < 8 {
			return nil, nil, fmt.Errorf("%w: truncated control field", ErrFrame)
		}
		field, n := binary.BigEndian.Uint32(rest[:4]), binary.BigEndian.Uint32(rest[4:8])
		rest = rest[8:]
		if field != fstrmFieldContentType {
			return nil, nil, fmt.Errorf("%w: control field type %d", ErrFrame, field)
		}
		if n > fstrmMaxContentType || uint64(n) > uint64(len(rest)) {
			return nil, nil, fmt.Errorf("%w: content type of %d bytes", ErrFrame, n)
		}
		c.ContentTypes = append(c.ContentTypes, string(rest[:n]))
		rest = rest[n:]
	}
	switch {
	case c.Type == fstrmStart && len(c.ContentTypes) > 1:
		return nil, nil, fmt.Errorf("%w: START with %d content types", ErrFrame, len(c.ContentTypes))
	case (c.Type == fstrmStop || c.Type == fstrmFinish) && len(c.ContentTypes) > 0:
		return nil, nil, fmt.Errorf("%w: control type %d with fields", ErrFrame, c.Type)
	}
	return nil, c, nil
}

// control reads one frame and requires a control frame of type want.
func (fr *FrameReader) control(want uint32) (*Control, error) {
	data, c, err := fr.Next()
	switch {
	case err != nil:
		return nil, err
	case data != nil:
		return nil, fmt.Errorf("%w: a data frame where control type %d belongs", ErrFrame, want)
	case c.Type != want:
		return nil, fmt.Errorf("%w: control type %d where %d belongs", ErrFrame, c.Type, want)
	}
	return c, nil
}

func (fr *FrameReader) word() (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(fr.r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// controlFrame encodes a control frame with its escape.
func controlFrame(typ uint32, contentTypes ...string) []byte {
	body := binary.BigEndian.AppendUint32(nil, typ)
	for _, ct := range contentTypes {
		body = binary.BigEndian.AppendUint32(body, fstrmFieldContentType)
		body = binary.BigEndian.AppendUint32(body, uint32(len(ct))) //nolint:gosec // G115: a content type is short
		body = append(body, ct...)
	}
	out := binary.BigEndian.AppendUint32(nil, 0)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body))) //nolint:gosec // G115: a control frame is short
	return append(out, body...)
}
