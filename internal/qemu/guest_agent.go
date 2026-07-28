package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"time"
)

// GuestAgentAvailable synchronizes the QEMU guest-agent stream before
// reporting it available. QGA has no greeting, so a socket connect alone is
// not evidence that the agent in the guest is responsive.
func GuestAgentAvailable(ctx context.Context, path string) bool {
	if path == "" {
		return false
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return false
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Second)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return false
	}
	const syncID = uint64(0x746f706f6c6162)
	payload, err := json.Marshal(map[string]any{
		"execute":   "guest-sync-delimited",
		"arguments": map[string]any{"id": syncID},
	})
	if err != nil {
		return false
	}
	request := []byte{0xff}
	request = append(request, payload...)
	request = append(request, '\n')
	if _, err := conn.Write(request); err != nil {
		return false
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return false
		}
		for len(line) > 0 && line[0] == 0xff {
			line = line[1:]
		}
		var response struct {
			Return *uint64 `json:"return"`
		}
		if json.Unmarshal(line, &response) == nil && response.Return != nil && *response.Return == syncID {
			return true
		}
	}
}
