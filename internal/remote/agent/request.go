package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/wire"
)

var (
	hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// readOnlyCommands run synchronously in the session and change no state.
var readOnlyCommands = map[string]bool{"ps": true, "logs": true, "counters": true}

// requestError is a validation failure with a stable code.
type requestError struct{ code, message string }

func (e *requestError) Error() string { return e.code + ": " + e.message }

func reqErr(code, format string, args ...any) error {
	return &requestError{code: code, message: fmt.Sprintf(format, args...)}
}

// validateRequest checks every field of an untrusted request.
func validateRequest(req wire.Request) error {
	total := len(req.Topology) + len(req.Command)
	for _, arg := range req.Args {
		total += len(arg)
	}
	for _, f := range req.RootCAs {
		if len(f.Content) > wire.MaxRootCAFile {
			return reqErr(wire.CodeRequestTooLarge, "Root CA file %q exceeds %d bytes", f.Path, wire.MaxRootCAFile)
		}
		total += len(f.Content) + len(f.Path)
	}
	for name, v := range req.Env {
		total += len(name) + len(v.Value)
	}
	if total > wire.MaxRequest {
		return reqErr(wire.CodeRequestTooLarge, "request exceeds %d bytes", wire.MaxRequest)
	}
	if req.JobID != "" && !hex32.MatchString(req.JobID) {
		return reqErr(wire.CodeProtocolError, "invalid job identifier")
	}
	for _, t := range req.Templates {
		if t.Name == "" || !hex64.MatchString(t.SHA256) || t.Size < 0 {
			return reqErr(wire.CodeProtocolError, "invalid template reference %q", t.Name)
		}
	}
	return nil
}

// resolveWorkingDir applies the working-directory rules on the host.
func resolveWorkingDir(value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) || strings.ContainsRune(value, 0) {
		return "", reqErr(wire.CodeWorkingDirInvalid, "working directory %q must be an absolute path", value)
	}
	resolved, err := config.ResolveWorkingDir(filepath.Clean(value))
	if err != nil {
		return "", reqErr(wire.CodeWorkingDirInvalid, "%v", err)
	}
	return resolved, nil
}

// requestFS serves Root CA files from the request and reports every other path
// as a regular file: template files exist only on the controller, and the
// controller has already validated them.
type requestFS struct{ files map[string][]byte }

type syntheticInfo struct {
	name string
	size int64
}

func (i syntheticInfo) Name() string       { return i.name }
func (i syntheticInfo) Size() int64        { return i.size }
func (i syntheticInfo) Mode() fs.FileMode  { return 0o444 }
func (i syntheticInfo) ModTime() time.Time { return time.Time{} }
func (i syntheticInfo) IsDir() bool        { return false }
func (i syntheticInfo) Sys() any           { return nil }

func (r requestFS) Stat(path string) (fs.FileInfo, error) {
	data := r.files[path]
	return syntheticInfo{name: filepath.Base(path), size: int64(len(data))}, nil
}

func (r requestFS) ReadFile(path string) ([]byte, error) {
	data, ok := r.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return data, nil
}

// parsed is the result of parsing a request's topology.
type parsed struct {
	cfg        *config.Config
	warnings   []string
	workingDir string
}

// parseRequest parses the topology as the controller did. The request's
// variables are the only interpolation source; an unlisted name, a parse
// failure, or a different normalized configuration is a protocol_error.
func parseRequest(req wire.Request) (*parsed, error) {
	wd, err := resolveWorkingDir(req.WorkingDir)
	if err != nil {
		return nil, err
	}
	if req.Topology == "" {
		return nil, reqErr(wire.CodeProtocolError, "request has no topology")
	}
	var unknown []string
	lookup := func(name string) (string, bool) {
		v, ok := req.Env[name]
		if !ok {
			unknown = append(unknown, name)
			return "", false
		}
		return v.Value, v.Set
	}
	files := map[string][]byte{}
	for _, f := range req.RootCAs {
		files[f.Path] = f.Content
	}
	cfg, warnings, err := config.ParseWith([]byte(req.Topology), config.ParseOptions{BaseDir: req.BaseDir, Lookup: lookup, FS: requestFS{files: files}})
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, reqErr(wire.CodeProtocolError, "env_divergence: topology looked up variables the request does not list: %s", strings.Join(unknown, ", "))
	}
	if err != nil {
		return nil, reqErr(wire.CodeProtocolError, "the agent could not parse a topology that the controller accepted: %v", err)
	}
	if req.ConfigDigest != "" {
		digest, err := config.Digest(cfg)
		if err != nil {
			return nil, err
		}
		if digest != req.ConfigDigest {
			return nil, reqErr(wire.CodeProtocolError, "the agent's normalized configuration differs from the controller's")
		}
	}
	if err := config.CheckWorkingDir(wd, cfg); err != nil {
		return nil, reqErr(wire.CodeWorkingDirInvalid, "%v", err)
	}
	return &parsed{cfg: cfg, warnings: warnings, workingDir: wd}, nil
}

func helperOverrides(h wire.Helpers) app.HelperOverrides {
	return app.HelperOverrides{QEMUSystem: h.QEMUSystem, QEMUImg: h.QEMUImg, LVM: h.LVM, OVMFCode: h.OVMFCode, OVMFVars: h.OVMFVars, Xorriso: h.Xorriso}
}

// handle runs the request and returns the exit status to report. A non-nil
// error means the stream failed and no exit frame can be sent.
func (s *session) handle(req wire.Request) (int, error) {
	if err := validateRequest(req); err != nil {
		return s.reject(err)
	}
	switch {
	case readOnlyCommands[req.Command]:
		return s.runReadOnly(req)
	case lifecycleCommands[req.Command]:
		return s.runLifecycle(req)
	case adminCommands[req.Command]:
		return s.runAdmin(req)
	case execCommands[req.Command]:
		return s.runExec(req)
	case req.Command == "capture":
		return s.runCapture(req)
	default:
		return s.reject(reqErr(wire.CodeProtocolError, "unknown command %q", req.Command))
	}
}

// reject sends an error frame for err and reports status 1 without a second
// exit frame: the caller's exit frame ends the session.
func (s *session) reject(err error) (int, error) {
	var re *requestError
	if errors.As(err, &re) {
		return 1, s.conn.WriteJSON(wire.TypeError, wire.ErrorMsg{Code: re.code, Message: re.message})
	}
	return 1, s.conn.WriteJSON(wire.TypeError, wire.ErrorMsg{Code: wire.CodeProtocolError, Message: err.Error()})
}

// runReadOnly runs a command that changes no state. The session cancels it
// when the controller cancels or the connection ends.
func (s *session) runReadOnly(req wire.Request) (int, error) {
	p, err := parseRequest(req)
	if err != nil {
		return s.reject(err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.setCancel(cancel)
	stdout := streamWriter{conn: s.conn, typ: wire.TypeStdout}
	stderr := streamWriter{conn: s.conn, typ: wire.TypeStderr}
	for _, w := range p.warnings {
		if _, err := fmt.Fprintf(stderr, "warning: %s\n", w); err != nil {
			return 1, err
		}
	}
	command := app.Command{Name: req.Command, Args: req.Args, Topology: p.cfg, WorkingDir: p.workingDir, Helpers: helperOverrides(req.Helpers)}
	runErr := s.opts.Runner.Run(ctx, command, RunIO{Stdout: stdout, Stderr: stderr})
	return s.finish(runErr, stderr)
}

// finish maps a command result to an exit status and prints errors the way
// local mode does.
func (s *session) finish(runErr error, stderr streamWriter) (int, error) {
	if runErr == nil {
		return 0, s.streamError()
	}
	var child *app.ChildExitError
	if errors.As(runErr, &child) {
		return child.Code, nil
	}
	if !errors.Is(runErr, context.Canceled) {
		if _, err := fmt.Fprintf(stderr, "topolab: %v\n", runErr); err != nil {
			return 1, err
		}
	}
	return 1, nil
}

// streamError reports a lost stream as an error so that no exit frame follows.
func (s *session) streamError() error {
	if cause := context.Cause(s.ctx); cause != nil && s.ctx.Err() != nil {
		return cause
	}
	return nil
}
