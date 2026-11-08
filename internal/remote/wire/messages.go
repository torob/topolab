package wire

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Error codes that the agent reports (spec.md Section 12.8).
const (
	CodeAgentMissing      = "agent_missing"
	CodeAgentMismatch     = "agent_mismatch"
	CodePrivilegeDenied   = "privilege_denied"
	CodeProtocolError     = "protocol_error"
	CodeRequestTooLarge   = "request_too_large"
	CodeWorkingDirInvalid = "working_dir_invalid"
	CodeJobActive         = "job_active"
	CodeJobNotFound       = "job_not_found"
	CodeBlobDigest        = "blob_digest_mismatch"
	CodeBlobInvalid       = "blob_invalid"
	CodeBlobCorrupt       = "blob_corrupt"
	CodeBlobNoSpace       = "blob_no_space"
	CodeHostTemplate      = "host_template_invalid"
)

// Request limits (spec.md Section 12.4).
const (
	MaxRootCAFile = 1 << 20
	MaxRequest    = 8 << 20
	BlobChunk     = 4 << 20
	BlobWindow    = 4
)

// Hello opens a session in each direction.
type Hello struct {
	Protocol int    `json:"protocol"`
	Digest   string `json:"digest"`
	Nonce    string `json:"nonce,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	Version  string `json:"version,omitempty"`
}

// EnvValue is one interpolation variable that the topology looked up.
type EnvValue struct {
	Set   bool   `json:"set"`
	Value string `json:"value,omitempty"`
}

// TemplateRef names a template that at least one VM uses.
type TemplateRef struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// RootCAFile carries a Root CA file that a VM names by path.
type RootCAFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// Helpers carry the global helper overrides; they name host paths.
type Helpers struct {
	QEMUSystem string `json:"qemu_system,omitempty"`
	QEMUImg    string `json:"qemu_img,omitempty"`
	LVM        string `json:"lvm,omitempty"`
	OVMFCode   string `json:"ovmf_code,omitempty"`
	OVMFVars   string `json:"ovmf_vars,omitempty"`
	Xorriso    string `json:"xorriso,omitempty"`
}

// TTY describes the controller's terminal for exec.
type TTY struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
	Term string `json:"term,omitempty"`
}

// Request is the one request of a session.
type Request struct {
	JobID      string              `json:"job_id,omitempty"`
	Command    string              `json:"command"`
	Args       []string            `json:"args,omitempty"`
	Topology   string              `json:"topology,omitempty"`
	BaseDir    string              `json:"base_dir,omitempty"`
	Env        map[string]EnvValue `json:"env,omitempty"`
	WorkingDir string              `json:"working_dir,omitempty"`
	Helpers    Helpers             `json:"helpers,omitempty"`
	Templates  []TemplateRef       `json:"templates,omitempty"`
	RootCAs    []RootCAFile        `json:"root_cas,omitempty"`
	TTY        *TTY                `json:"tty,omitempty"`
	// ConfigDigest is the SHA-256 of the controller's normalized configuration.
	// The agent compares it with its own before it changes anything.
	ConfigDigest string `json:"config_digest,omitempty"`
	// Offset is the first output record to send for a job attach.
	Offset int64 `json:"offset,omitempty"`
}

// Resize reports a new terminal size.
type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Signal names a signal to forward: INT, TERM, HUP, or QUIT.
type Signal struct {
	Name string `json:"name"`
}

// Blob states reported in BlobState.State.
const (
	BlobPresent = "present"
	BlobPartial = "partial"
	BlobAbsent  = "absent"
)

// BlobState is the agent's view of one template digest.
type BlobState struct {
	SHA256 string `json:"sha256"`
	State  string `json:"state"`
	Offset int64  `json:"offset,omitempty"`
}

// BlobStatus answers the template list of a request.
type BlobStatus struct {
	Blobs []BlobState `json:"blobs"`
}

// BlobEnd closes the upload of one digest.
type BlobEnd struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// BlobAck confirms bytes up to Offset.
type BlobAck struct {
	SHA256 string `json:"sha256"`
	Offset int64  `json:"offset"`
}

// BlobDone is the agent's verdict on one upload.
type BlobDone struct {
	SHA256  string `json:"sha256"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// JobNotice announces a job and its state.
type JobNotice struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// ErrorMsg is an agent-reported error; an Exit frame follows it.
type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Exit is the final frame from the agent.
type Exit struct {
	Code int    `json:"code"`
	Job  string `json:"job,omitempty"`
}

// EncodeBlobData builds the payload of a blob-data frame: the raw digest, the
// big-endian offset, then the bytes.
func EncodeBlobData(digest string, offset int64, data []byte) ([]byte, error) {
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("%w: invalid blob digest %q", ErrProtocol, digest)
	}
	out := make([]byte, 40, 40+len(data))
	copy(out, raw)
	binary.BigEndian.PutUint64(out[32:40], uint64(offset))
	return append(out, data...), nil
}

// DecodeBlobData splits a blob-data payload.
func DecodeBlobData(payload []byte) (digest string, offset int64, data []byte, err error) {
	if len(payload) < 40 {
		return "", 0, nil, fmt.Errorf("%w: short blob-data frame", ErrProtocol)
	}
	return hex.EncodeToString(payload[:32]), int64(binary.BigEndian.Uint64(payload[32:40])), payload[40:], nil
}
