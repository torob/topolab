package client

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/wire"
)

// RecordingLookup wraps an environment lookup and remembers every name that
// interpolation asks for, including names that are unset. Only these names
// leave the controller (spec.md Section 12.4).
type RecordingLookup struct {
	Base     func(string) (string, bool)
	recorded map[string]wire.EnvValue
}

// Lookup is the function to give to the configuration loader.
func (r *RecordingLookup) Lookup(name string) (string, bool) {
	value, ok := r.Base(name)
	if r.recorded == nil {
		r.recorded = map[string]wire.EnvValue{}
	}
	r.recorded[name] = wire.EnvValue{Set: ok, Value: value}
	return value, ok
}

// Recorded returns a copy of the recorded variables.
func (r *RecordingLookup) Recorded() map[string]wire.EnvValue {
	out := make(map[string]wire.EnvValue, len(r.recorded))
	for k, v := range r.recorded {
		out[k] = v
	}
	return out
}

// RemoteError is an error that the agent reported.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("remote: %s: %s", e.Code, e.Message)
}

// BuildRequest builds the request for an invocation. It fails with a
// RemoteError (request_too_large) when a limit is exceeded.
func BuildRequest(inv Invocation) (wire.Request, error) {
	req := wire.Request{
		JobID:      inv.JobID,
		Command:    inv.Command,
		Args:       inv.Args,
		Topology:   string(inv.TopologyData),
		BaseDir:    inv.BaseDir,
		Env:        inv.Env,
		WorkingDir: inv.WorkingDir,
		Helpers:    inv.Helpers,
		TTY:        inv.TTY,
	}
	if inv.Topology != nil {
		digest, err := config.Digest(inv.Topology)
		if err != nil {
			return req, err
		}
		req.ConfigDigest = digest
		files, err := rootCAFiles(inv.Topology)
		if err != nil {
			return req, err
		}
		req.RootCAs = files
	}
	req.Templates = inv.Templates
	payload, err := json.Marshal(req)
	if err != nil {
		return req, err
	}
	if len(payload) > wire.MaxRequest {
		return req, &RemoteError{Code: wire.CodeRequestTooLarge, Message: fmt.Sprintf("request of %d bytes exceeds %d", len(payload), wire.MaxRequest)}
	}
	return req, nil
}

// rootCAFiles collects the Root CA files that VMs name by path. The loader has
// already read and validated them, and CanonicalPEM is their canonical form,
// so no file is read a second time.
func rootCAFiles(cfg *config.Config) ([]wire.RootCAFile, error) {
	seen := map[string][]byte{}
	for _, vm := range cfg.VMs {
		for _, cert := range vm.RootCACertificates {
			if cert == nil || cert.Path == "" {
				continue
			}
			content := []byte(cert.CanonicalPEM)
			if len(content) > wire.MaxRootCAFile {
				return nil, &RemoteError{Code: wire.CodeRequestTooLarge, Message: fmt.Sprintf("Root CA file %q exceeds %d bytes", cert.Path, wire.MaxRootCAFile)}
			}
			seen[cert.Path] = content
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	files := make([]wire.RootCAFile, 0, len(paths))
	for _, path := range paths {
		files = append(files, wire.RootCAFile{Path: path, Content: seen[path]})
	}
	return files, nil
}
