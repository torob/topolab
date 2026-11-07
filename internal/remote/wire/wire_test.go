package wire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf, &buf)
	if err := c.WriteJSON(TypeHello, Hello{Protocol: Protocol, Digest: "ab"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(TypeStdout, []byte("output")); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(TypeStdin, nil); err != nil {
		t.Fatal(err)
	}
	f, err := c.Read()
	if err != nil || f.Type != TypeHello {
		t.Fatalf("first frame = %v, %v", f, err)
	}
	var hello Hello
	if err := f.JSON(&hello); err != nil || hello.Protocol != Protocol || hello.Digest != "ab" {
		t.Fatalf("hello = %+v, %v", hello, err)
	}
	f, err = c.Read()
	if err != nil || f.Type != TypeStdout || string(f.Payload) != "output" {
		t.Fatalf("second frame = %v, %v", f, err)
	}
	f, err = c.Read()
	if err != nil || f.Type != TypeStdin || len(f.Payload) != 0 {
		t.Fatalf("third frame = %v, %v", f, err)
	}
	if _, err := c.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream error = %v", err)
	}
}

func TestWriteRejectsOversizePayload(t *testing.T) {
	c := NewConn(strings.NewReader(""), io.Discard)
	err := c.Write(TypeStdout, make([]byte, MaxPayload+1))
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v", err)
	}
	if err := c.Write(TypeStdout, make([]byte, MaxPayload)); err != nil {
		t.Fatalf("payload of exactly MaxPayload: %v", err)
	}
}

func TestReadRejectsMalformedFrames(t *testing.T) {
	tests := map[string][]byte{
		"zero type":    {0, 0, 0, 0, 0},
		"unknown type": {byte(typeLimit), 0, 0, 0, 0},
		"oversize":     {byte(TypeStdout), 0x00, 0x80, 0x00, 0x01},
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewConn(bytes.NewReader(raw), io.Discard).Read()
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestReadDistinguishesCleanAndTruncatedEnd(t *testing.T) {
	var buf bytes.Buffer
	if err := NewConn(strings.NewReader(""), &buf).Write(TypeStdout, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	whole := buf.Bytes()
	for cut := 1; cut < len(whole); cut++ {
		_, err := NewConn(bytes.NewReader(whole[:cut]), io.Discard).Read()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut at %d: err = %v, want unexpected EOF", cut, err)
		}
	}
	if _, err := NewConn(bytes.NewReader(nil), io.Discard).Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream err = %v", err)
	}
}

func TestConcurrentWritesStayWhole(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(strings.NewReader(""), &buf)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				_ = c.Write(TypeStdout, bytes.Repeat([]byte{'x'}, 100))
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	reader := NewConn(&buf, io.Discard)
	for i := 0; i < 400; i++ {
		f, err := reader.Read()
		if err != nil || len(f.Payload) != 100 {
			t.Fatalf("frame %d: %v %d", i, err, len(f.Payload))
		}
	}
}

func TestBlobDataRoundTrip(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	payload, err := EncodeBlobData(digest, 1<<40, []byte("chunk"))
	if err != nil {
		t.Fatal(err)
	}
	gotDigest, offset, data, err := DecodeBlobData(payload)
	if err != nil || gotDigest != digest || offset != 1<<40 || string(data) != "chunk" {
		t.Fatalf("decoded %q %d %q %v", gotDigest, offset, data, err)
	}
	if _, err := EncodeBlobData("zz", 0, nil); err == nil {
		t.Fatal("invalid digest accepted")
	}
	if _, _, _, err := DecodeBlobData(make([]byte, 39)); err == nil {
		t.Fatal("short payload accepted")
	}
}

func TestScanPreambleSkipsLoginNoise(t *testing.T) {
	nonce, err := NewNonce()
	if err != nil || len(nonce) != 32 {
		t.Fatalf("nonce = %q, %v", nonce, err)
	}
	stream := "Welcome to host\nlast login: x\n" + string(Preamble(nonce)) + "rest"
	r := bufio.NewReader(strings.NewReader(stream))
	noise, err := ScanPreamble(r, nonce, MaxPreamble)
	if err != nil {
		t.Fatal(err)
	}
	if string(noise) != "Welcome to host\nlast login: x\n" {
		t.Fatalf("noise = %q", noise)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "rest" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestScanPreambleFailures(t *testing.T) {
	nonce := strings.Repeat("0", 32)
	if _, err := ScanPreamble(bufio.NewReader(strings.NewReader("no preamble here")), nonce, MaxPreamble); !errors.Is(err, ErrProtocol) {
		t.Fatalf("missing preamble err = %v", err)
	}
	other := strings.Repeat("1", 32)
	if _, err := ScanPreamble(bufio.NewReader(strings.NewReader(string(Preamble(other)))), nonce, MaxPreamble); !errors.Is(err, ErrProtocol) {
		t.Fatalf("wrong nonce err = %v", err)
	}
	long := strings.Repeat("n", 300) + string(Preamble(nonce))
	if _, err := ScanPreamble(bufio.NewReader(strings.NewReader(long)), nonce, 100); !errors.Is(err, ErrProtocol) {
		t.Fatalf("noise over the limit err = %v", err)
	}
	exact := strings.Repeat("n", 100) + string(Preamble(nonce))
	if _, err := ScanPreamble(bufio.NewReader(strings.NewReader(exact)), nonce, 100); err != nil {
		t.Fatalf("noise at the limit err = %v", err)
	}
}
