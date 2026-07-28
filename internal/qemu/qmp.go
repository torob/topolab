package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

type QMPClient struct {
	path   string
	dial   DialFunc
	mu     sync.Mutex
	conn   net.Conn
	dec    *json.Decoder
	enc    *json.Encoder
	nextID uint64
}

func NewQMPClient(path string, dial DialFunc) *QMPClient {
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	return &QMPClient{path: path, dial: dial}
}

type qmpEnvelope struct {
	QMP    json.RawMessage `json:"QMP,omitempty"`
	Return json.RawMessage `json:"return,omitempty"`
	Error  *struct {
		Class       string `json:"class"`
		Description string `json:"desc"`
	} `json:"error,omitempty"`
	Event string `json:"event,omitempty"`
	ID    uint64 `json:"id,omitempty"`
}

func (c *QMPClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked(ctx)
}

func (c *QMPClient) connectLocked(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	conn, err := c.dial(ctx, "unix", c.path)
	if err != nil {
		return fmt.Errorf("connect QMP %s: %w", c.path, err)
	}
	c.conn, c.dec, c.enc = conn, json.NewDecoder(bufio.NewReader(conn)), json.NewEncoder(conn)
	if err := setDeadline(conn, ctx); err != nil {
		c.closeLocked()
		return err
	}
	var greeting qmpEnvelope
	if err := c.dec.Decode(&greeting); err != nil {
		c.closeLocked()
		return fmt.Errorf("read QMP greeting: %w", err)
	}
	if len(greeting.QMP) == 0 {
		c.closeLocked()
		return errors.New("invalid QMP greeting")
	}
	if _, err := c.executeLocked(ctx, "qmp_capabilities", nil); err != nil {
		c.closeLocked()
		return err
	}
	return nil
}

func (c *QMPClient) Execute(ctx context.Context, command string, arguments any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(ctx); err != nil {
		return err
	}
	raw, err := c.executeLocked(ctx, command, arguments)
	if err != nil {
		return err
	}
	if result != nil && len(raw) != 0 && string(raw) != "{}" && string(raw) != "null" {
		if err := json.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("decode QMP %s response: %w", command, err)
		}
	}
	return nil
}

func (c *QMPClient) executeLocked(ctx context.Context, command string, arguments any) (json.RawMessage, error) {
	if err := setDeadline(c.conn, ctx); err != nil {
		return nil, err
	}
	c.nextID++
	id := c.nextID
	req := map[string]any{"execute": command, "id": id}
	if arguments != nil {
		req["arguments"] = arguments
	}
	if err := c.enc.Encode(req); err != nil {
		c.closeLocked()
		return nil, fmt.Errorf("send QMP %s: %w", command, err)
	}
	for {
		var response qmpEnvelope
		if err := c.dec.Decode(&response); err != nil {
			c.closeLocked()
			return nil, fmt.Errorf("read QMP %s: %w", command, err)
		}
		if response.Event != "" || response.ID != id {
			continue
		}
		if response.Error != nil {
			return nil, fmt.Errorf("QMP %s: %s: %s", command, response.Error.Class, response.Error.Description)
		}
		return response.Return, nil
	}
}

func (c *QMPClient) Balloon(ctx context.Context, targetBytes uint64) error {
	return c.Execute(ctx, "balloon", map[string]any{"value": targetBytes}, nil)
}
func (c *QMPClient) BlockResize(ctx context.Context, blockNode string, sizeBytes uint64) error {
	if blockNode == "" {
		return errors.New("block node is required")
	}
	return c.Execute(ctx, "block_resize", map[string]any{"node-name": blockNode, "size": sizeBytes}, nil)
}
func (c *QMPClient) Powerdown(ctx context.Context) error {
	return c.Execute(ctx, "system_powerdown", nil, nil)
}
func (c *QMPClient) Quit(ctx context.Context) error { return c.Execute(ctx, "quit", nil, nil) }

func (c *QMPClient) QueryBalloon(ctx context.Context) (uint64, error) {
	var value struct {
		Actual uint64 `json:"actual"`
	}
	if err := c.Execute(ctx, "query-balloon", nil, &value); err != nil {
		return 0, err
	}
	return value.Actual, nil
}
func (c *QMPClient) QueryStatus(ctx context.Context) (string, error) {
	var value struct {
		Status string `json:"status"`
	}
	if err := c.Execute(ctx, "query-status", nil, &value); err != nil {
		return "", err
	}
	return value.Status, nil
}

func (c *QMPClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.dec = nil
	c.enc = nil
	return err
}
func (c *QMPClient) closeLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.dec = nil
	c.enc = nil
}

func setDeadline(conn net.Conn, ctx context.Context) error {
	if conn == nil {
		return errors.New("not connected")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	return conn.SetDeadline(deadline)
}
