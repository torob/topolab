// Package observability implements persistent serial JSONL, pcapng output,
// and stable directional link-counter representations.
package observability

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type SerialRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
	Node      string    `json:"-"`
}
type ListenFunc func(network, address string) (net.Listener, error)

type SerialCollector struct {
	Node, SocketPath, LogPath string
	Listen                    ListenFunc
	Now                       func() time.Time
	MaxLineBytes              int
}

func (c *SerialCollector) Serve(ctx context.Context) error {
	if c.Listen == nil {
		c.Listen = net.Listen
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.MaxLineBytes <= 0 {
		c.MaxLineBytes = 1 << 20
	}
	if err := os.MkdirAll(filepath.Dir(c.SocketPath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.LogPath), 0o700); err != nil {
		return err
	}
	if st, err := os.Lstat(c.SocketPath); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket %s", c.SocketPath)
		}
		if err := os.Remove(c.SocketPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := c.Listen("unix", c.SocketPath)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close(); _ = os.Remove(c.SocketPath) }()
	if err := os.Chmod(c.SocketPath, 0o600); err != nil {
		return err
	}
	log, err := os.OpenFile(c.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		err = c.consume(conn, log)
		_ = conn.Close()
		if err != nil {
			return err
		}
	}
}

func (c *SerialCollector) consume(r io.Reader, log io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), c.MaxLineBytes)
	enc := json.NewEncoder(log)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if err := enc.Encode(SerialRecord{Timestamp: c.Now().UTC(), Line: line}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func ReadSerial(path, node string, tail int) ([]SerialRecord, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var records []SerialRecord
	for scanner.Scan() {
		var r SerialRecord
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		r.Node = node
		records = append(records, r)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if tail >= 0 && len(records) > tail {
		records = records[len(records)-tail:]
	}
	return records, nil
}

func MergeSerial(perNode map[string][]SerialRecord) []SerialRecord {
	var out []SerialRecord
	for node, records := range perNode {
		for _, r := range records {
			r.Node = node
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Node < out[j].Node
		}
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}
