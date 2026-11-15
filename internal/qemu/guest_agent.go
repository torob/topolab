package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// guestAgentSyncID is the id that TopoLab sends in guest-sync-delimited.
const guestAgentSyncID = uint64(0x746f706f6c6162)

// GuestAgentAvailable synchronizes the QEMU guest-agent stream before
// reporting it available. QGA has no greeting, so a socket connect alone is
// not evidence that the agent in the guest is responsive.
func GuestAgentAvailable(ctx context.Context, path string) bool {
	if path == "" {
		return false
	}
	conn, _, err := openGuestAgent(ctx, path, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// GuestAgentExecute runs one guest-agent command without arguments and
// returns its result. It synchronizes the stream first, so an old reply that
// another client did not read cannot be taken as the reply. Without a context
// deadline it waits up to 30 seconds.
func GuestAgentExecute(ctx context.Context, path, command string) (json.RawMessage, error) {
	if path == "" {
		return nil, errors.New("guest-agent socket is not configured")
	}
	conn, reader, err := openGuestAgent(ctx, path, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	payload, err := json.Marshal(map[string]any{"execute": command})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("send guest-agent %s: %w", command, err)
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read guest-agent %s: %w", command, err)
		}
		line = trimDelimiters(line)
		var response struct {
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Class       string `json:"class"`
				Description string `json:"desc"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &response) != nil {
			continue
		}
		if response.Error != nil {
			return nil, fmt.Errorf("guest-agent %s: %s: %s", command, response.Error.Class, response.Error.Description)
		}
		if response.Return != nil {
			return response.Return, nil
		}
	}
}

// openGuestAgent connects to the guest-agent socket and synchronizes the
// stream with guest-sync-delimited. The returned reader continues after the
// sync reply.
func openGuestAgent(ctx context.Context, path string, fallback time.Duration) (net.Conn, *bufio.Reader, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("connect guest agent %s: %w", path, err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(fallback)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"execute":   "guest-sync-delimited",
		"arguments": map[string]any{"id": guestAgentSyncID},
	})
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	request := []byte{0xff}
	request = append(request, payload...)
	request = append(request, '\n')
	if _, err := conn.Write(request); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("synchronize guest agent: %w", err)
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("synchronize guest agent: %w", err)
		}
		var response struct {
			Return *uint64 `json:"return"`
		}
		if json.Unmarshal(trimDelimiters(line), &response) == nil && response.Return != nil && *response.Return == guestAgentSyncID {
			return conn, reader, nil
		}
	}
}

// trimDelimiters removes the 0xff bytes that the agent sends before the
// guest-sync-delimited reply.
func trimDelimiters(line []byte) []byte {
	for len(line) > 0 && line[0] == 0xff {
		line = line[1:]
	}
	return line
}
