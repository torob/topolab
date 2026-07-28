package network

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// DeviceName creates a stable Linux interface name within IFNAMSIZ. The kind
// prefix is truncated to three bytes and the remaining bytes are a hash.
func DeviceName(kind string, parts ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("topolab/network-name/v1\x00"))
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	if len(kind) > 3 {
		kind = kind[:3]
	}
	n := 15 - len(kind)
	return kind + hex.EncodeToString(h.Sum(nil))[:n]
}

func namespaceName(topology, router string) string {
	return "topolab-" + DeviceName("ns", topology, router)
}

func FabricNamespaceName(topology string) string {
	return namespaceName(topology, "fabric")
}

func ManagementHostNamespaceName(topology string) string {
	return namespaceName(topology, "management-host")
}

func nftTableName(topology string) string { return DeviceName("tl", topology, "nft") }

func ownerAlias(topology, kind, logical string) string {
	return fmt.Sprintf("topolab:v1:%s:%s:%s", topology, kind, logical)
}

func randomGenerationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
