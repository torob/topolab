package wire

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const preambleMagic = "TOPOLAB-AGENT-PROTOCOL-1 "

// NewNonce returns a random 128-bit nonce as 32 lowercase hexadecimal digits.
func NewNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// Preamble returns the bytes that the agent writes after it reads the
// controller's hello. They contain the controller's nonce.
func Preamble(nonce string) []byte {
	return []byte(preambleMagic + nonce + "\n")
}

// ScanPreamble reads from r until the preamble for nonce appears. It returns
// the bytes that came before it, such as output of login scripts. It fails when
// more than limit bytes precede the preamble or the stream ends first.
func ScanPreamble(r *bufio.Reader, nonce string, limit int) ([]byte, error) {
	want := Preamble(nonce)
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: agent preamble not found: %v", ErrProtocol, err)
		}
		buf = append(buf, b)
		if bytes.HasSuffix(buf, want) {
			return buf[:len(buf)-len(want)], nil
		}
		if len(buf) > limit+len(want) {
			return nil, fmt.Errorf("%w: agent preamble not found within %d bytes", ErrProtocol, limit)
		}
	}
}
