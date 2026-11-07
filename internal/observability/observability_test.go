package observability

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type packetSequence struct{ remaining int }

func (s *packetSequence) ReadPacket(ctx context.Context) (Packet, error) {
	if err := ctx.Err(); err != nil {
		return Packet{}, err
	}
	if s.remaining == 0 {
		return Packet{}, io.EOF
	}
	s.remaining--
	return Packet{Interface: 0, Direction: Egress, Timestamp: time.Now(), Data: make([]byte, 60)}, nil
}
func (*packetSequence) Close() error { return nil }

type blockingPacketSource struct{}

func (*blockingPacketSource) ReadPacket(ctx context.Context) (Packet, error) {
	<-ctx.Done()
	return Packet{}, ctx.Err()
}
func (*blockingPacketSource) Close() error { return nil }

func TestPcapNGAndVLANReconstruction(t *testing.T) {
	var b bytes.Buffer
	w, err := NewPcapNGWriter(&b, 64, []InterfaceDescription{{Name: "lab/link/node:eth0/egress", Direction: Egress}})
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 60)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	ts := time.Unix(1, 2).UTC()
	if err := w.WritePacket(Packet{Interface: 0, Direction: Egress, Timestamp: ts, Data: frame, VLAN: &VLANTag{TPID: 0x8100, TCI: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	if binary.LittleEndian.Uint32(raw[:4]) != 0x0A0D0D0A {
		t.Fatal("missing section header")
	}
	if !bytes.Contains(raw, []byte{0x81, 0x00, 0x00, 0x0a, 0x08, 0x00}) {
		t.Fatal("VLAN tag not reconstructed")
	}
}

func TestSerialRecords(t *testing.T) {
	var b bytes.Buffer
	times := []time.Time{time.Unix(1, 0), time.Unix(2, 0)}
	i := 0
	c := SerialCollector{Now: func() time.Time { v := times[i]; i++; return v }}
	if err := c.consume(strings.NewReader("one\r\ntwo\n"), &b); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatal(lines)
	}
	var r SerialRecord
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil {
		t.Fatal(err)
	}
	if r.Line != "one" || !r.Timestamp.Equal(times[0]) {
		t.Fatalf("%+v", r)
	}
}

func TestCounterDeltaRejectsGenerationChange(t *testing.T) {
	before := CounterReport{Topology: "lab", Links: []LinkCounters{{Name: "x", GenerationID: "a"}}}
	after := before
	after.Links = append([]LinkCounters(nil), before.Links...)
	after.Links[0].GenerationID = "b"
	if _, err := Delta(before, after); err == nil {
		t.Fatal("expected generation error")
	}
}

func TestCounterDelta(t *testing.T) {
	before := CounterReport{Topology: "lab", Links: []LinkCounters{{Name: "x", GenerationID: "a", Endpoints: []EndpointCounters{{Node: "n", Interface: "i", Ingress: TrafficCounters{Packets: U64(2)}}}}}}
	after := CounterReport{Topology: "lab", Links: []LinkCounters{{Name: "x", GenerationID: "a", Endpoints: []EndpointCounters{{Node: "n", Interface: "i", Ingress: TrafficCounters{Packets: U64(7)}}}}}}
	delta, err := Delta(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if got := *delta.Links[0].Endpoints[0].Ingress.Packets; got != 5 {
		t.Fatalf("delta %d", got)
	}
}

func TestCaptureWritesExactlyCountPackets(t *testing.T) {
	var out bytes.Buffer
	w, err := NewPcapNGWriter(&out, 64, []InterfaceDescription{{Name: "egress", Direction: Egress}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Capture(context.Background(), w, []PacketSource{&packetSequence{remaining: 100}}, CaptureOptions{Count: 3}); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()
	offset, packets := 0, 0
	for offset+12 <= len(data) {
		kind := binary.LittleEndian.Uint32(data[offset:])
		size := int(binary.LittleEndian.Uint32(data[offset+4:]))
		if size < 12 || offset+size > len(data) {
			t.Fatalf("invalid pcapng framing at %d", offset)
		}
		if kind == 6 {
			packets++
		}
		offset += size
	}
	if packets != 3 {
		t.Fatalf("wrote %d packets, want 3", packets)
	}
}

func TestCaptureFailsWhenOwnershipChanges(t *testing.T) {
	var out bytes.Buffer
	w, err := NewPcapNGWriter(&out, 64, []InterfaceDescription{{Name: "egress", Direction: Egress}})
	if err != nil {
		t.Fatal(err)
	}
	err = Capture(context.Background(), w, []PacketSource{&blockingPacketSource{}}, CaptureOptions{
		OwnershipCheck: func(context.Context) error { return errors.New("wrong alias") },
		CheckInterval:  time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// writeRecorder remembers every Write call separately.
type writeRecorder struct{ writes [][]byte }

func (r *writeRecorder) Write(p []byte) (int, error) {
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

func TestPcapNGWritesEachBlockWithOneWrite(t *testing.T) {
	var rec writeRecorder
	w, err := NewPcapNGWriter(&rec, 64, []InterfaceDescription{{Name: "a", Direction: Ingress}, {Name: "b", Direction: Egress}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WritePacket(Packet{Interface: 1, Direction: Egress, Timestamp: time.Unix(5, 6), Data: make([]byte, 61)}); err != nil {
		t.Fatal(err)
	}
	// One section header, two interface descriptions, and one packet block.
	if len(rec.writes) != 4 {
		t.Fatalf("got %d writes, want 4", len(rec.writes))
	}
	for i, block := range rec.writes {
		if len(block) < 12 || len(block)%4 != 0 {
			t.Fatalf("write %d has length %d", i, len(block))
		}
		total := binary.LittleEndian.Uint32(block[4:8])
		trailer := binary.LittleEndian.Uint32(block[len(block)-4:])
		if int(total) != len(block) || trailer != total {
			t.Fatalf("write %d is not one whole block: total=%d trailer=%d len=%d", i, total, trailer, len(block))
		}
	}
}
