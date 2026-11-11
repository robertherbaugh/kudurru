package dnstap

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame Streams (https://farsightsec.github.io/fstrm/), written from the
// protocol description: a stream of frames, each a 32-bit big-endian length
// and that many bytes of data. A length of zero escapes a control frame: a
// second length, then a 32-bit control type and its fields, each a 32-bit
// field type, a 32-bit length and that many bytes. The only field type is
// CONTENT_TYPE. The bidirectional handshake of a socket writer is READY
// (writer, with the content types it offers), ACCEPT (reader, with the ones
// it takes), START (writer, with the one it sends), data frames, STOP
// (writer) and FINISH (reader).

// ContentType is the Frame Streams content type of dnstap.
const ContentType = "protobuf:dnstap.Dnstap"

// Control frame types.
const (
	ControlAccept uint32 = 0x01
	ControlStart  uint32 = 0x02
	ControlStop   uint32 = 0x03
	ControlReady  uint32 = 0x04
	ControlFinish uint32 = 0x05
)

// controlFieldContentType is the CONTENT_TYPE field of a control frame.
const controlFieldContentType uint32 = 0x01

// Bounds of a frame.
const (
	// MaxControlLength bounds a control frame, as the reference
	// implementation does.
	MaxControlLength = 512
	// MaxContentTypeLength bounds one content type within it.
	MaxContentTypeLength = 256
	// MaxDataLength bounds a data frame the reader accepts. A dnstap
	// message holds at most two DNS messages of 64 KiB each.
	MaxDataLength = 1 << 20
)

// Errors of a frame stream.
var (
	ErrFrameTooLarge = errors.New("frame streams: frame exceeds its bound")
	ErrMalformed     = errors.New("frame streams: malformed control frame")
)

// Control is a decoded control frame.
type Control struct {
	Type         uint32
	ContentTypes []string
}

// AppendControl appends a control frame (with its escape) to b.
func AppendControl(b []byte, typ uint32, contentTypes ...string) []byte {
	length := 4
	for _, ct := range contentTypes {
		length += 8 + len(ct)
	}
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(length)) //nolint:gosec // G115: bounded by the content types the caller passes
	b = binary.BigEndian.AppendUint32(b, typ)
	for _, ct := range contentTypes {
		b = binary.BigEndian.AppendUint32(b, controlFieldContentType)
		b = binary.BigEndian.AppendUint32(b, uint32(len(ct))) //nolint:gosec // G115: a content type is short
		b = append(b, ct...)
	}
	return b
}

// AppendData appends a data frame holding payload to b. An empty payload
// cannot be framed (its length would be the escape), so it appends nothing.
func AppendData(b, payload []byte) []byte {
	if len(payload) == 0 {
		return b
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(payload))) //nolint:gosec // G115: a dnstap message is far below 4 GiB
	return append(b, payload...)
}

// ParseControl decodes the body of a control frame: the type and its
// fields, without the escape and the length.
func ParseControl(body []byte) (Control, error) {
	if len(body) < 4 || len(body) > MaxControlLength {
		return Control{}, ErrMalformed
	}
	c := Control{Type: binary.BigEndian.Uint32(body)}
	rest := body[4:]
	for len(rest) > 0 {
		if len(rest) < 8 {
			return Control{}, ErrMalformed
		}
		field, n := binary.BigEndian.Uint32(rest), binary.BigEndian.Uint32(rest[4:])
		rest = rest[8:]
		if field != controlFieldContentType || n > MaxContentTypeLength || int(n) > len(rest) {
			return Control{}, ErrMalformed
		}
		c.ContentTypes = append(c.ContentTypes, string(rest[:n]))
		rest = rest[n:]
	}
	return c, nil
}

// Reader reads the frames of one stream.
type Reader struct {
	r *bufio.Reader
	// MaxData bounds a data frame; zero means MaxDataLength.
	MaxData int
}

// NewReader reads frames from r.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReader(r)} }

// Next returns the next frame: data, or a control frame when control is
// not nil. A data slice is valid until the next call.
func (fr *Reader) Next(buf []byte) (data []byte, control *Control, err error) {
	var head [4]byte
	if _, err := io.ReadFull(fr.r, head[:]); err != nil {
		return nil, nil, err
	}
	length := binary.BigEndian.Uint32(head[:])
	if length == 0 {
		if _, err := io.ReadFull(fr.r, head[:]); err != nil {
			return nil, nil, noEOF(err)
		}
		length = binary.BigEndian.Uint32(head[:])
		if length > MaxControlLength {
			return nil, nil, ErrFrameTooLarge
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(fr.r, body); err != nil {
			return nil, nil, noEOF(err)
		}
		c, err := ParseControl(body)
		if err != nil {
			return nil, nil, err
		}
		return nil, &c, nil
	}
	limit := fr.MaxData
	if limit <= 0 {
		limit = MaxDataLength
	}
	if uint64(length) > uint64(limit) {
		return nil, nil, ErrFrameTooLarge
	}
	if cap(buf) < int(length) {
		buf = make([]byte, length)
	}
	buf = buf[:length]
	if _, err := io.ReadFull(fr.r, buf); err != nil {
		return nil, nil, noEOF(err)
	}
	return buf, nil, nil
}

// ReadControl reads one frame and requires a control frame of type want.
func (fr *Reader) ReadControl(want uint32) (Control, error) {
	_, c, err := fr.Next(nil)
	if err != nil {
		return Control{}, err
	}
	if c == nil {
		return Control{}, fmt.Errorf("frame streams: a data frame where control frame %d was expected", want)
	}
	if c.Type != want {
		return Control{}, fmt.Errorf("frame streams: control frame %d where %d was expected", c.Type, want)
	}
	return *c, nil
}

// HasContentType reports whether c names ct.
func (c Control) HasContentType(ct string) bool {
	for _, have := range c.ContentTypes {
		if have == ct {
			return true
		}
	}
	return false
}

func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
