package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// DeterministicMAC returns a stable locally administered unicast MAC.
func DeterministicMAC(topology, node, iface string) string {
	sum := sha256.Sum256([]byte("topolab/mac/v1\x00" + topology + "\x00" + node + "\x00" + iface))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// LinuxName creates a deterministic kernel interface name no longer than 15
// bytes. kind should be a short semantic prefix such as "tlv" or "tlb".
func LinuxName(kind string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte("topolab/name/v1\x00"))
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	suffix := hex.EncodeToString(h.Sum(nil)[:6])
	if len(kind) > 3 {
		kind = kind[:3]
	}
	return kind + suffix[:15-len(kind)]
}

// DeterministicUUID returns a stable RFC 4122 variant, version-5-shaped UUID
// derived with SHA-256. It is suitable for QEMU node identity.
func DeterministicUUID(topology, node string) string {
	s := sha256.Sum256([]byte("topolab/uuid/v1\x00" + topology + "\x00" + node))
	b := s[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
