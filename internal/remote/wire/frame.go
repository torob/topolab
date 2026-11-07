// Package wire defines the private frame protocol between a controller and its
// agent. Both ends are the same executable, so the layout is not a public
// contract (spec.md Section 12.2).
package wire

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	// Protocol is the protocol number that this specification defines.
	Protocol = 1
	// MaxPayload is the largest accepted frame payload.
	MaxPayload = 8 << 20
	// MaxPreamble is how many bytes the controller skips before the preamble.
	MaxPreamble = 64 << 10
)

// Type identifies a frame kind.
type Type uint8

// Frame kinds. The numeric values are private to identical executables.
const (
	TypeHello Type = iota + 1
	TypeRequest
	TypeStdin
	TypeStdout
	TypeStderr
	TypeResize
	TypeSignal
	TypeCancel
	TypePing
	TypePong
	TypeBlobStatus
	TypeBlobData
	TypeBlobEnd
	TypeBlobAck
	TypeBlobDone
	TypeJobNotice
	TypeError
	TypeExit
	typeLimit
)

var typeNames = map[Type]string{
	TypeHello: "hello", TypeRequest: "request", TypeStdin: "stdin", TypeStdout: "stdout",
	TypeStderr: "stderr", TypeResize: "resize", TypeSignal: "signal", TypeCancel: "cancel",
	TypePing: "ping", TypePong: "pong", TypeBlobStatus: "blob-status", TypeBlobData: "blob-data",
	TypeBlobEnd: "blob-end", TypeBlobAck: "blob-ack", TypeBlobDone: "blob-done",
	TypeJobNotice: "job-notice", TypeError: "error", TypeExit: "exit",
}

func (t Type) String() string {
	if name, ok := typeNames[t]; ok {
		return name
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// ErrProtocol marks a malformed or oversized frame.
var ErrProtocol = errors.New("protocol error")

// Frame is one typed message.
type Frame struct {
	Type    Type
	Payload []byte
}

// JSON decodes the payload into v.
func (f Frame) JSON(v any) error {
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return fmt.Errorf("%w: decode %s frame: %v", ErrProtocol, f.Type, err)
	}
	return nil
}

// Conn reads and writes frames. Writes are serialized, so any goroutine may
// write; reads must come from one goroutine.
type Conn struct {
	r   *bufio.Reader
	w   io.Writer
	wmu sync.Mutex
}

// NewConn wraps a byte stream. When r is already a *bufio.Reader it is used as
// is, so bytes that a preamble scan buffered are not lost.
func NewConn(r io.Reader, w io.Writer) *Conn {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 64<<10)
	}
	return &Conn{r: br, w: w}
}

// Write sends one frame. A payload larger than MaxPayload is an error.
func (c *Conn) Write(t Type, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("%w: %s payload of %d bytes exceeds %d", ErrProtocol, t, len(payload), MaxPayload)
	}
	buf := make([]byte, 5, 5+len(payload))
	buf[0] = byte(t)
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	buf = append(buf, payload...)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.w.Write(buf)
	return err
}

// WriteJSON sends a frame whose payload is the JSON encoding of v.
func (c *Conn) WriteJSON(t Type, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(t, payload)
}

// Read returns the next frame. A clean end of stream is io.EOF; a stream cut
// inside a frame is io.ErrUnexpectedEOF.
func (c *Conn) Read() (Frame, error) {
	var header [5]byte
	if _, err := io.ReadFull(c.r, header[:]); err != nil {
		return Frame{}, err
	}
	t := Type(header[0])
	if t == 0 || t >= typeLimit {
		return Frame{}, fmt.Errorf("%w: unknown frame type %d", ErrProtocol, header[0])
	}
	size := binary.BigEndian.Uint32(header[1:5])
	if size > MaxPayload {
		return Frame{}, fmt.Errorf("%w: %s frame of %d bytes exceeds %d", ErrProtocol, t, size, MaxPayload)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Type: t, Payload: payload}, nil
}
