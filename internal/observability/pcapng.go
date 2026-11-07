package observability

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

type Direction string

const (
	Ingress Direction = "ingress"
	Egress  Direction = "egress"
)

type InterfaceDescription struct {
	Name, Comment string
	Direction     Direction
}
type Packet struct {
	Interface      int
	Direction      Direction
	Timestamp      time.Time
	Data           []byte
	OriginalLength uint32
	VLAN           *VLANTag
}
type VLANTag struct{ TPID, TCI uint16 }

type PcapNGWriter struct {
	mu         sync.Mutex
	w          io.Writer
	snaplen    uint32
	interfaces []InterfaceDescription
	closed     bool
}

func NewPcapNGWriter(w io.Writer, snaplen uint32, interfaces []InterfaceDescription) (*PcapNGWriter, error) {
	if w == nil {
		return nil, errors.New("nil pcapng writer")
	}
	if snaplen < 64 || snaplen > 262144 {
		return nil, fmt.Errorf("snaplen %d outside 64..262144", snaplen)
	}
	if len(interfaces) == 0 {
		return nil, errors.New("at least one pcapng interface is required")
	}
	p := &PcapNGWriter{w: w, snaplen: snaplen, interfaces: append([]InterfaceDescription(nil), interfaces...)}
	if err := p.writeSection(); err != nil {
		return nil, err
	}
	for _, d := range interfaces {
		if err := p.writeInterface(d); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *PcapNGWriter) WritePacket(packet Packet) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("pcapng writer is closed")
	}
	if packet.Interface < 0 || packet.Interface >= len(p.interfaces) {
		return errors.New("invalid pcapng interface index")
	}
	data := packet.Data
	if packet.VLAN != nil {
		data = InsertVLANTag(data, *packet.VLAN)
	}
	orig := packet.OriginalLength
	if orig == 0 {
		orig = uint32(len(data))
	}
	if len(data) > int(p.snaplen) {
		data = data[:p.snaplen]
	}
	ns := packet.Timestamp.UTC().UnixNano()
	flags := uint32(1)
	if packet.Direction == Egress {
		flags = 2
	}
	body := make([]byte, 20)
	binary.LittleEndian.PutUint32(body[0:4], uint32(packet.Interface))
	binary.LittleEndian.PutUint32(body[4:8], uint32(uint64(ns)>>32))
	binary.LittleEndian.PutUint32(body[8:12], uint32(ns))
	binary.LittleEndian.PutUint32(body[12:16], uint32(len(data)))
	binary.LittleEndian.PutUint32(body[16:20], orig)
	body = append(body, data...)
	body = pad4(body)
	body = appendOption(body, 2, uint32Bytes(flags))
	body = appendOption(body, 0, nil)
	return writeBlock(p.w, 6, body)
}
func (p *PcapNGWriter) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if s, ok := p.w.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return err
		}
	}
	if c, ok := p.w.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (p *PcapNGWriter) writeSection() error {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[0:4], 0x1A2B3C4D)
	binary.LittleEndian.PutUint16(body[4:6], 1)
	binary.LittleEndian.PutUint16(body[6:8], 0)
	binary.LittleEndian.PutUint64(body[8:16], ^uint64(0))
	return writeBlock(p.w, 0x0A0D0D0A, body)
}
func (p *PcapNGWriter) writeInterface(d InterfaceDescription) error {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[0:2], 1)
	binary.LittleEndian.PutUint32(body[4:8], p.snaplen)
	body = appendOption(body, 2, []byte(d.Name))
	body = appendOption(body, 3, []byte(d.Comment))
	body = appendOption(body, 9, []byte{9})
	body = appendOption(body, 0, nil)
	return writeBlock(p.w, 1, body)
}

// writeBlock writes one complete block with a single Write call, so a writer
// that frames or forwards each call never splits a block.
func writeBlock(w io.Writer, typ uint32, body []byte) error {
	total := uint32(len(body) + 12)
	block := make([]byte, 0, total)
	block = binary.LittleEndian.AppendUint32(block, typ)
	block = binary.LittleEndian.AppendUint32(block, total)
	block = append(block, body...)
	block = binary.LittleEndian.AppendUint32(block, total)
	_, err := w.Write(block)
	return err
}
func appendOption(dst []byte, code uint16, value []byte) []byte {
	var h [4]byte
	binary.LittleEndian.PutUint16(h[0:2], code)
	binary.LittleEndian.PutUint16(h[2:4], uint16(len(value)))
	dst = append(dst, h[:]...)
	dst = append(dst, value...)
	return pad4(dst)
}
func pad4(v []byte) []byte {
	for len(v)%4 != 0 {
		v = append(v, 0)
	}
	return v
}
func uint32Bytes(v uint32) []byte { var b [4]byte; binary.LittleEndian.PutUint32(b[:], v); return b[:] }

// InsertVLANTag reconstructs a tag stripped by host receive offload. It does
// not add a duplicate tag if the frame already carries the supplied TPID.
func InsertVLANTag(frame []byte, tag VLANTag) []byte {
	if len(frame) < 14 {
		return append([]byte(nil), frame...)
	}
	if tag.TPID == 0 {
		tag.TPID = 0x8100
	}
	current := binary.BigEndian.Uint16(frame[12:14])
	if current == 0x8100 || current == 0x88a8 {
		return append([]byte(nil), frame...)
	}
	out := make([]byte, len(frame)+4)
	copy(out[:12], frame[:12])
	binary.BigEndian.PutUint16(out[12:14], tag.TPID)
	binary.BigEndian.PutUint16(out[14:16], tag.TCI)
	copy(out[16:], frame[12:])
	return out
}
