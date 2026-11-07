package jobs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Output record streams.
const (
	StreamStdout byte = 1
	StreamStderr byte = 2
	StreamExit   byte = 3
)

// Log limits (spec.md Section 12.6).
const (
	MaxRecordPayload = 64 << 10
	MaxLogSize       = 64 << 20
	recordHeader     = 5
)

const truncationMarker = "topolab: output log limit reached; further output is not recorded\n"

// LogPath is the path of a job's output log.
func (s *Store) LogPath(topology, id string) string {
	return filepath.Join(s.Dir(topology, id), "output.log")
}

// LogWriter appends output records. It is safe for concurrent use.
type LogWriter struct {
	mu     sync.Mutex
	f      *os.File
	size   int64
	capped bool
	limit  int64
}

// OpenLog opens a job's log for appending.
func (s *Store) OpenLog(topology, id string) (*LogWriter, error) {
	f, err := os.OpenFile(s.LogPath(topology, id), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &LogWriter{f: f, size: info.Size(), limit: MaxLogSize}, nil
}

// SetLimit changes the size limit; tests use a small one.
func (w *LogWriter) SetLimit(n int64) { w.limit = n }

func (w *LogWriter) record(stream byte, payload []byte) error {
	buf := make([]byte, recordHeader, recordHeader+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	buf[4] = stream
	buf = append(buf, payload...)
	n, err := w.f.Write(buf)
	w.size += int64(n)
	return err
}

// Write appends output on a stream, split into records of at most 64 KiB. After
// the size limit it records one truncation marker and drops further output.
func (w *LogWriter) Write(stream byte, p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(p)
	for len(p) > 0 {
		if w.capped {
			return total, nil
		}
		part := p
		if len(part) > MaxRecordPayload {
			part = part[:MaxRecordPayload]
		}
		if w.size+int64(recordHeader+len(part)) > w.limit {
			w.capped = true
			if err := w.record(StreamStderr, []byte(truncationMarker)); err != nil {
				return total - len(p), err
			}
			return total, nil
		}
		if err := w.record(stream, part); err != nil {
			return total - len(p), err
		}
		p = p[len(part):]
	}
	return total, nil
}

// WriteExit appends the final record. It is exempt from the size limit.
func (w *LogWriter) WriteExit(code int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.record(StreamExit, []byte(fmt.Sprintf("%d", code)))
}

// Close closes the log.
func (w *LogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// StreamWriter returns a writer for one stream.
func (w *LogWriter) StreamWriter(stream byte) io.Writer { return streamWriter{w: w, stream: stream} }

type streamWriter struct {
	w      *LogWriter
	stream byte
}

func (s streamWriter) Write(p []byte) (int, error) { return s.w.Write(s.stream, p) }

// Record is one output record.
type Record struct {
	Stream  byte
	Payload []byte
	// End is the offset just after the record.
	End int64
}

// ReadRecord reads the record that starts at offset. It reports false, without
// an error, when the log does not yet hold a complete record there.
func ReadRecord(path string, offset int64) (Record, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, false, err
	}
	defer f.Close()
	var header [recordHeader]byte
	if _, err := f.ReadAt(header[:], offset); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	size := int64(binary.BigEndian.Uint32(header[0:4]))
	if size > MaxRecordPayload {
		return Record{}, false, fmt.Errorf("corrupt output log: record of %d bytes at offset %d", size, offset)
	}
	payload := make([]byte, size)
	if _, err := f.ReadAt(payload, offset+recordHeader); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	return Record{Stream: header[4], Payload: payload, End: offset + recordHeader + size}, true, nil
}

// RecordSize is the size of a record with a payload of n bytes.
func RecordSize(n int) int64 { return int64(recordHeader + n) }
