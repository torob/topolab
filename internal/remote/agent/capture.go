package agent

import (
	"context"
	"io"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/remote/wire"
)

// blockSink sends each write as one frame. The pcapng writer writes one whole
// block per call, so a stream that is cut leaves the controller's file with
// complete blocks only (spec.md Section 12.7).
type blockSink struct{ conn *wire.Conn }

func (b blockSink) Write(p []byte) (int, error) {
	if err := b.conn.Write(wire.TypeStdout, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (b blockSink) Close() error { return nil }

// runCapture streams a packet capture to the controller. A cancel frame ends
// the capture normally, as an interrupt does in local mode.
func (s *session) runCapture(req wire.Request) (int, error) {
	p, err := parseRequest(req)
	if err != nil {
		return s.reject(err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.setCancel(cancel)
	stdout := streamWriter{conn: s.conn, typ: wire.TypeStdout}
	stderr := streamWriter{conn: s.conn, typ: wire.TypeStderr}
	command := app.Command{Name: "capture", Args: req.Args, Topology: p.cfg, WorkingDir: p.workingDir, Helpers: helperOverrides(req.Helpers)}
	runErr := s.opts.Runner.Run(ctx, command, RunIO{
		Stdout: stdout,
		Stderr: stderr,
		CaptureSink: func() (io.WriteCloser, error) {
			return blockSink{conn: s.conn}, nil
		},
	})
	return s.finish(runErr, stderr)
}
