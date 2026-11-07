package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/blob"
	"github.com/torob/topolab/internal/remote/jobs"
	"github.com/torob/topolab/internal/remote/wire"
)

// liveProcs reports every runner as alive, as the test process stands in for it.
type liveProcs struct {
	jobs.OSProcs
	dead atomic.Bool
}

func (p *liveProcs) StartTime(pid int) (uint64, error) {
	if p.dead.Load() {
		return 0, os.ErrNotExist
	}
	return p.OSProcs.StartTime(pid)
}

func (p *liveProcs) Cmdline(int) ([]string, error) { return []string{"topolab", "__job"}, nil }

// jobLab runs job runners in this process.
type jobLab struct {
	t       *testing.T
	runner  *fakeRunner
	procs   *liveProcs
	wd      string
	mu      sync.Mutex
	cancels []context.CancelFunc
	running sync.WaitGroup
	spawned int
}

func newJobLab(t *testing.T, run func(ctx context.Context, command app.Command, rio RunIO) error) *jobLab {
	t.Helper()
	wd, err := os.MkdirTemp("/tmp", "tlag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(wd) })
	lab := &jobLab{t: t, runner: &fakeRunner{run: run}, procs: &liveProcs{}, wd: wd}
	t.Cleanup(func() { lab.cancelAll(); lab.running.Wait() })
	return lab
}

func (l *jobLab) cancelAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.cancels {
		c()
	}
}

func (l *jobLab) options() Options {
	return Options{
		Runner:       l.runner,
		JobProcs:     l.procs,
		PollInterval: 5 * time.Millisecond,
		SpawnJob: func(dir string) error {
			ctx, cancel := context.WithCancel(context.Background())
			l.mu.Lock()
			l.cancels = append(l.cancels, cancel)
			l.spawned++
			l.mu.Unlock()
			l.running.Add(1)
			go func() {
				defer l.running.Done()
				RunJob(ctx, dir, l.runner)
			}()
			return nil
		},
		SignalRunner: func(int) { l.cancelAll() },
	}
}

func (l *jobLab) request(command string, args ...string) wire.Request {
	req := baseRequest(l.t, command, args...)
	req.WorkingDir = l.wd
	return req
}

func (l *jobLab) store() *jobs.Store {
	s := jobs.NewStore(l.wd)
	s.Procs = l.procs
	return s
}

// waitTerminal waits until the job's status is final. The runner writes the
// exit record first and the final state after it, as the specification orders.
func (l *jobLab) waitTerminal(topology, id string) *jobs.Status {
	l.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := l.store().Load(topology, id)
		if err != nil {
			l.t.Fatal(err)
		}
		if jobs.Terminal(st.State) {
			// secrets.json is deleted just after the state is written.
			for time.Now().Before(deadline) {
				if _, err := os.Stat(filepath.Join(l.store().Dir(topology, id), "secrets.json")); os.IsNotExist(err) {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			return st
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("job %s stayed %s", id, st.State)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func jobID(n int) string { return strings.Repeat(fmt.Sprintf("%x", n), 32) }

// collectJob reads frames to the exit frame and also returns the job notice.
func (p *peer) collectJob() (stdout, stderr string, errMsg *wire.ErrorMsg, exit wire.Exit, notice *wire.JobNotice) {
	p.t.Helper()
	var out, errb strings.Builder
	for {
		f, err := p.conn.Read()
		if err != nil {
			p.t.Fatalf("read: %v (stdout=%q stderr=%q)", err, out.String(), errb.String())
		}
		switch f.Type {
		case wire.TypeStdout:
			out.Write(f.Payload)
		case wire.TypeStderr:
			errb.Write(f.Payload)
		case wire.TypeError:
			var m wire.ErrorMsg
			_ = f.JSON(&m)
			errMsg = &m
		case wire.TypeJobNotice:
			var n wire.JobNotice
			_ = f.JSON(&n)
			notice = &n
		case wire.TypeExit:
			_ = f.JSON(&exit)
			return out.String(), errb.String(), errMsg, exit, notice
		case wire.TypePing:
			_ = p.conn.Write(wire.TypePong, nil)
		}
	}
}

func TestLifecycleCommandRunsAsAJobAndStreamsItsOutput(t *testing.T) {
	lab := newJobLab(t, func(_ context.Context, _ app.Command, rio RunIO) error {
		fmt.Fprint(rio.Stdout, "stopping vm\n")
		fmt.Fprint(rio.Stderr, "note\n")
		return nil
	})
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("stop")
	req.JobID = jobID(1)
	if err := p.conn.WriteJSON(wire.TypeRequest, req); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, errMsg, exit, notice := p.collectJob()
	if errMsg != nil || exit.Code != 0 || exit.Job != req.JobID || stdout != "stopping vm\n" || stderr != "note\n" {
		t.Fatalf("stdout=%q stderr=%q err=%v exit=%+v", stdout, stderr, errMsg, exit)
	}
	if notice == nil || notice.ID != req.JobID {
		t.Fatalf("notice = %+v", notice)
	}
	st := lab.waitTerminal("lists", req.JobID)
	if st.State != jobs.StateSucceeded || st.ExitCode != 0 || st.Command != "stop" || st.Started == nil || st.Finished == nil || st.Runner.PID != os.Getpid() {
		t.Fatalf("status = %+v", st)
	}
	if _, err := lab.store().Secrets("lists", req.JobID); !os.IsNotExist(err) {
		t.Fatalf("secrets remain: %v", err)
	}
	if cmd := lab.runner.commands[0]; cmd.Name != "stop" || cmd.Topology.Name != "lists" {
		t.Fatalf("command = %+v", cmd)
	}
}

func TestFailedJobReportsTheErrorAndState(t *testing.T) {
	lab := newJobLab(t, func(context.Context, app.Command, RunIO) error { return errors.New("network is absent") })
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("start")
	req.JobID = jobID(2)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	_, stderr, errMsg, exit, _ := p.collectJob()
	if errMsg != nil || exit.Code != 1 || stderr != "topolab: network is absent\n" {
		t.Fatalf("stderr=%q err=%v exit=%+v", stderr, errMsg, exit)
	}
	st := lab.waitTerminal("lists", req.JobID)
	if st.State != jobs.StateFailed || st.ExitCode != 1 || st.Error != "network is absent" {
		t.Fatalf("status = %+v", st)
	}
}

func TestTemplateDigestFailureIsReportedAsBlobCorrupt(t *testing.T) {
	lab := newJobLab(t, func(context.Context, app.Command, RunIO) error {
		return fmt.Errorf("template base: %w: x", app.ErrTemplateDigest)
	})
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("restart")
	req.JobID = jobID(3)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	_, stderr, _, exit, _ := p.collectJob()
	if exit.Code != 1 || !strings.Contains(stderr, "blob_corrupt:") {
		t.Fatalf("stderr=%q exit=%+v", stderr, exit)
	}
}

func TestCancelFrameStopsTheRunnerAndTheJobEndsCancelled(t *testing.T) {
	started := make(chan struct{})
	lab := newJobLab(t, func(ctx context.Context, _ app.Command, rio RunIO) error {
		fmt.Fprint(rio.Stdout, "working\n")
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("down")
	req.JobID = jobID(4)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	<-started
	if err := p.conn.Write(wire.TypeCancel, nil); err != nil {
		t.Fatal(err)
	}
	stdout, _, errMsg, exit, _ := p.collectJob()
	if errMsg != nil || exit.Code != 1 || stdout != "working\n" {
		t.Fatalf("stdout=%q err=%v exit=%+v", stdout, errMsg, exit)
	}
	st := lab.waitTerminal("lists", req.JobID)
	if st.State != jobs.StateCancelled {
		t.Fatalf("state = %s", st.State)
	}
}

func TestConnectionLossLeavesTheJobRunningAndReattachResumesWithoutRepeats(t *testing.T) {
	release := make(chan struct{})
	first := make(chan struct{})
	lab := newJobLab(t, func(_ context.Context, _ app.Command, rio RunIO) error {
		fmt.Fprint(rio.Stdout, "part one\n")
		close(first)
		<-release
		fmt.Fprint(rio.Stdout, "part two\n")
		return nil
	})
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("up")
	req.JobID = jobID(5)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	<-first
	// Read until the first output arrives, noting the record boundary.
	var offset int64
	for offset == 0 {
		f, err := p.conn.Read()
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == wire.TypeStdout {
			offset += jobs.RecordSize(len(f.Payload))
		}
	}
	closeInput(p) // the connection drops
	if err := <-p.done; err == nil {
		t.Fatal("Serve reported success after a lost connection")
	}
	st, _ := lab.store().Load("lists", req.JobID)
	if jobs.Terminal(st.State) {
		t.Fatalf("the job ended with the connection: %+v", st)
	}

	close(release)
	// Reconnect: the same request resumes from the recorded boundary.
	q := startAgent(t, lab.options())
	q.handshake("")
	req.Offset = offset
	_ = q.conn.WriteJSON(wire.TypeRequest, req)
	stdout, _, errMsg, exit, notice := q.collectJob()
	if errMsg != nil || exit.Code != 0 || stdout != "part two\n" || notice == nil {
		t.Fatalf("stdout=%q err=%v exit=%+v", stdout, errMsg, exit)
	}
	if lab.spawned != 1 {
		t.Fatalf("a reconnect started %d runners, want 1", lab.spawned)
	}
	// A full replay shows everything.
	r := startAgent(t, lab.options())
	r.handshake("")
	req.Offset = 0
	_ = r.conn.WriteJSON(wire.TypeRequest, req)
	stdout, _, _, _, _ = r.collectJob()
	if stdout != "part one\npart two\n" {
		t.Fatalf("replay = %q", stdout)
	}
}

func TestSecondLifecycleCommandIsRefusedWhileAJobRuns(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	lab := newJobLab(t, func(context.Context, app.Command, RunIO) error {
		close(started)
		<-release
		return nil
	})
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("up")
	req.JobID = jobID(6)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	<-started

	q := startAgent(t, lab.options())
	q.handshake("")
	other := lab.request("down")
	other.JobID = jobID(7)
	_ = q.conn.WriteJSON(wire.TypeRequest, other)
	_, _, errMsg, exit, _ := q.collectJob()
	if errMsg == nil || errMsg.Code != wire.CodeJobActive || exit.Code != 1 || !strings.Contains(errMsg.Message, req.JobID) {
		t.Fatalf("err=%+v exit=%+v", errMsg, exit)
	}
	if lab.spawned != 1 || len(lab.runner.commands) != 1 {
		t.Fatalf("spawned=%d commands=%d", lab.spawned, len(lab.runner.commands))
	}
	if _, err := lab.store().Load("lists", other.JobID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("a refused job left records: %v", err)
	}
	close(release)
	if _, _, _, exit, _ := p.collectJob(); exit.Code != 0 {
		t.Fatalf("first job exit = %+v", exit)
	}
}

func TestReusedJobIdentifierWithAnotherRequestIsAConflict(t *testing.T) {
	lab := newJobLab(t, nil)
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("stop")
	req.JobID = jobID(8)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	p.collectJob()

	q := startAgent(t, lab.options())
	q.handshake("")
	other := lab.request("down")
	other.JobID = req.JobID
	_ = q.conn.WriteJSON(wire.TypeRequest, other)
	_, _, errMsg, exit, _ := q.collectJob()
	if errMsg == nil || errMsg.Code != wire.CodeProtocolError || !strings.Contains(errMsg.Message, "different request") || exit.Code != 1 {
		t.Fatalf("err=%+v exit=%+v", errMsg, exit)
	}
}

func TestVanishedRunnerIsMarkedLostAndTheStreamEnds(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once, releaseOnce sync.Once
	lab := newJobLab(t, func(context.Context, app.Command, RunIO) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	})
	defer releaseOnce.Do(func() { close(release) })
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("up")
	req.JobID = jobID(9)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	<-started
	lab.procs.dead.Store(true) // the runner "crashes" or the host reboots
	_, stderr, _, exit, _ := p.collectJob()
	if exit.Code != 1 || !strings.Contains(stderr, "is lost") {
		t.Fatalf("stderr=%q exit=%+v", stderr, exit)
	}
	st, _ := lab.store().Load("lists", req.JobID)
	if st.State != jobs.StateLost {
		t.Fatalf("state = %s", st.State)
	}
	// A lost job does not block the next command.
	lab.procs.dead.Store(false)
	releaseOnce.Do(func() { close(release) })
	q := startAgent(t, lab.options())
	q.handshake("")
	next := lab.request("up")
	next.JobID = jobID(10)
	_ = q.conn.WriteJSON(wire.TypeRequest, next)
	_, _, errMsg, _, _ := q.collectJob()
	if errMsg != nil && errMsg.Code == wire.CodeJobActive {
		t.Fatalf("a lost job still blocks: %+v", errMsg)
	}
}

func TestRunnerThatNeverStartsFailsTheJob(t *testing.T) {
	lab := newJobLab(t, nil)
	opts := lab.options()
	opts.SpawnJob = func(string) error { return nil }
	opts.JobStartTimeout = 80 * time.Millisecond
	p := startAgent(t, opts)
	p.handshake("")
	req := lab.request("up")
	req.JobID = jobID(11)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	_, _, errMsg, exit, _ := p.collectJob()
	if errMsg == nil || !strings.Contains(errMsg.Message, "did not start") || exit.Code != 1 {
		t.Fatalf("err=%+v exit=%+v", errMsg, exit)
	}
	st, _ := lab.store().Load("lists", req.JobID)
	if st.State != jobs.StateFailed {
		t.Fatalf("state = %s", st.State)
	}

	opts.SpawnJob = func(string) error { return errors.New("no such file") }
	q := startAgent(t, opts)
	q.handshake("")
	req.JobID = jobID(12)
	_ = q.conn.WriteJSON(wire.TypeRequest, req)
	_, _, errMsg, _, _ = q.collectJob()
	if errMsg == nil || !strings.Contains(errMsg.Message, "no such file") {
		t.Fatalf("spawn failure err = %+v", errMsg)
	}
}

// --- templates ---

const vmTopology = `name: lab
templates:
  - {name: base, path: /controller/base.qcow2}
storages:
  - {name: local, type: file, root_provisioning: snapshot}
vms:
  - name: vm
    template: base
    root_disk: {storage: local}
    ssh_authorized_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"]
    interfaces: [{name: eth0}]
switches: [{name: lan}]
links:
  - name: vm-lan
    endpoints:
      - {node: vm, interface: eth0}
      - {node: lan, interface: vm}
`

func qcow2Image(payload string) []byte {
	h := make([]byte, 24)
	copy(h, "QFI\xfb")
	binary.BigEndian.PutUint32(h[4:8], 3)
	return append(h, payload...)
}

func upRequest(t *testing.T, lab *jobLab, image []byte, jobN int) wire.Request {
	t.Helper()
	cfg, _, err := config.ParseWith([]byte(vmTopology), config.ParseOptions{BaseDir: "/controller", FS: requestFS{}})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := config.Digest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(image)
	return wire.Request{
		JobID: jobID(jobN), Command: "up", Topology: vmTopology, BaseDir: "/controller", WorkingDir: lab.wd, ConfigDigest: digest,
		Templates: []wire.TemplateRef{{Name: "base", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(image))}},
	}
}

// readStatus reads the blob status frame.
func (p *peer) readStatus() wire.BlobStatus {
	p.t.Helper()
	for {
		f, err := p.conn.Read()
		if err != nil {
			p.t.Fatal(err)
		}
		switch f.Type {
		case wire.TypeBlobStatus:
			var st wire.BlobStatus
			if err := f.JSON(&st); err != nil {
				p.t.Fatal(err)
			}
			return st
		case wire.TypePing:
			_ = p.conn.Write(wire.TypePong, nil)
		default:
			p.t.Fatalf("frame %s before blob status", f.Type)
		}
	}
}

// sendBlob uploads data from offset in chunks and returns the final blob-done frame.
func (p *peer) sendBlob(digest string, data []byte, offset int, chunk int) wire.BlobDone {
	p.t.Helper()
	for offset < len(data) {
		end := min(offset+chunk, len(data))
		payload, err := wire.EncodeBlobData(digest, int64(offset), data[offset:end])
		if err != nil {
			p.t.Fatal(err)
		}
		if err := p.conn.Write(wire.TypeBlobData, payload); err != nil {
			p.t.Fatal(err)
		}
		f := p.skipPings()
		if f.Type == wire.TypeBlobDone {
			var d wire.BlobDone
			_ = f.JSON(&d)
			return d
		}
		var ack wire.BlobAck
		if err := f.JSON(&ack); err != nil || f.Type != wire.TypeBlobAck || ack.Offset != int64(end) {
			p.t.Fatalf("ack = %v %+v %v", f.Type, ack, err)
		}
		offset = end
	}
	if err := p.conn.WriteJSON(wire.TypeBlobEnd, wire.BlobEnd{SHA256: digest, Size: int64(len(data))}); err != nil {
		p.t.Fatal(err)
	}
	f := p.skipPings()
	var d wire.BlobDone
	if f.Type != wire.TypeBlobDone {
		p.t.Fatalf("frame %s, want blob-done", f.Type)
	}
	_ = f.JSON(&d)
	return d
}

func (p *peer) skipPings() wire.Frame {
	p.t.Helper()
	for {
		f, err := p.conn.Read()
		if err != nil {
			p.t.Fatal(err)
		}
		if f.Type == wire.TypePing {
			_ = p.conn.Write(wire.TypePong, nil)
			continue
		}
		return f
	}
}

func TestUpReceivesTheTemplateOnceAndRunsAgainstTheBlob(t *testing.T) {
	var seen app.Command
	var digests map[string]string
	lab := newJobLab(t, func(_ context.Context, command app.Command, rio RunIO) error {
		seen, digests = command, rio.TemplateDigests
		return nil
	})
	image := qcow2Image(strings.Repeat("template bytes ", 400))
	req := upRequest(t, lab, image, 1)
	digest := req.Templates[0].SHA256

	p := startAgent(t, lab.options())
	p.handshake("")
	if err := p.conn.WriteJSON(wire.TypeRequest, req); err != nil {
		t.Fatal(err)
	}
	st := p.readStatus()
	if len(st.Blobs) != 1 || st.Blobs[0].SHA256 != digest || st.Blobs[0].State != wire.BlobAbsent {
		t.Fatalf("status = %+v", st)
	}
	if done := p.sendBlob(digest, image, 0, 1000); !done.OK {
		t.Fatalf("blob done = %+v", done)
	}
	_, _, errMsg, exit, _ := p.collectJob()
	if errMsg != nil || exit.Code != 0 {
		t.Fatalf("err=%v exit=%+v", errMsg, exit)
	}
	blobPath := blob.NewStore(lab.wd).Path(digest)
	if seen.Topology.Templates[0].Path != blobPath || digests[blobPath] != digest {
		t.Fatalf("template path = %q digests = %v, want %q", seen.Topology.Templates[0].Path, digests, blobPath)
	}
	if got, _ := os.ReadFile(blobPath); string(got) != string(image) {
		t.Fatal("stored blob differs from the upload")
	}
	st2 := lab.waitTerminal("lab", req.JobID)
	if len(st2.Templates) != 1 || st2.Templates[0] != digest {
		t.Fatalf("job templates = %v", st2.Templates)
	}

	// The next up finds the blob and receives no template byte.
	q := startAgent(t, lab.options())
	q.handshake("")
	again := upRequest(t, lab, image, 2)
	if err := q.conn.WriteJSON(wire.TypeRequest, again); err != nil {
		t.Fatal(err)
	}
	st = q.readStatus()
	if len(st.Blobs) != 1 || st.Blobs[0].State != wire.BlobPresent {
		t.Fatalf("second status = %+v", st)
	}
	_, _, errMsg, exit, _ = q.collectJob()
	if errMsg != nil || exit.Code != 0 {
		t.Fatalf("second up: err=%v exit=%+v", errMsg, exit)
	}
}

func TestInterruptedTemplateUploadResumesFromTheStoredOffset(t *testing.T) {
	lab := newJobLab(t, nil)
	image := qcow2Image(strings.Repeat("resume me ", 300))
	req := upRequest(t, lab, image, 1)
	digest := req.Templates[0].SHA256

	p := startAgent(t, lab.options())
	p.handshake("")
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	p.readStatus()
	half := len(image) / 2
	payload, _ := wire.EncodeBlobData(digest, 0, image[:half])
	_ = p.conn.Write(wire.TypeBlobData, payload)
	if f := p.skipPings(); f.Type != wire.TypeBlobAck {
		t.Fatalf("frame %s", f.Type)
	}
	closeInput(p)
	<-p.done

	q := startAgent(t, lab.options())
	q.handshake("")
	_ = q.conn.WriteJSON(wire.TypeRequest, req)
	st := q.readStatus()
	if len(st.Blobs) != 1 || st.Blobs[0].State != wire.BlobPartial || st.Blobs[0].Offset != int64(half) {
		t.Fatalf("resume status = %+v", st)
	}
	if done := q.sendBlob(digest, image, half, 700); !done.OK {
		t.Fatalf("done = %+v", done)
	}
	_, _, errMsg, exit, _ := q.collectJob()
	if errMsg != nil || exit.Code != 0 {
		t.Fatalf("err=%v exit=%+v", errMsg, exit)
	}
}

func TestBadTemplateUploadsFailWithTheirCodeAndCreateNoJob(t *testing.T) {
	image := qcow2Image(strings.Repeat("good ", 100))
	tests := map[string]struct {
		mutate func(req *wire.Request) []byte // returns the data to send
		code   string
	}{
		"digest mismatch": {func(req *wire.Request) []byte {
			return qcow2Image(strings.Repeat("evil ", 100))[:len(image)]
		}, wire.CodeBlobDigest},
		"not a qcow2 image": {func(req *wire.Request) []byte {
			data := []byte(strings.Repeat("plain text ", 50))
			sum := sha256.Sum256(data)
			req.Templates[0].SHA256, req.Templates[0].Size = hex.EncodeToString(sum[:]), int64(len(data))
			return data
		}, wire.CodeBlobInvalid},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			lab := newJobLab(t, nil)
			req := upRequest(t, lab, image, 1)
			data := tt.mutate(&req)
			p := startAgent(t, lab.options())
			p.handshake("")
			_ = p.conn.WriteJSON(wire.TypeRequest, req)
			p.readStatus()
			done := p.sendBlob(req.Templates[0].SHA256, data, 0, 600)
			if done.OK || done.Code != tt.code {
				t.Fatalf("done = %+v", done)
			}
			_, _, errMsg, exit, _ := p.collectJob()
			if errMsg == nil || errMsg.Code != tt.code || exit.Code != 1 {
				t.Fatalf("err=%+v exit=%+v", errMsg, exit)
			}
			if _, err := lab.store().Load("lab", req.JobID); !errors.Is(err, jobs.ErrNotFound) {
				t.Fatalf("a job was created: %v", err)
			}
			store := blob.NewStore(lab.wd)
			if st, _, _ := store.Status(req.Templates[0].SHA256); st != blob.StateAbsent {
				t.Fatalf("blob state after rejection = %s", st)
			}
		})
	}
}

func TestUpNeedsEveryUsedTemplateInTheRequest(t *testing.T) {
	lab := newJobLab(t, nil)
	req := upRequest(t, lab, qcow2Image("x"), 1)
	req.Templates = nil
	p := startAgent(t, lab.options())
	p.handshake("")
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	_, _, errMsg, exit, _ := p.collectJob()
	if errMsg == nil || errMsg.Code != wire.CodeProtocolError || !strings.Contains(errMsg.Message, "does not list it") || exit.Code != 1 {
		t.Fatalf("err=%+v exit=%+v", errMsg, exit)
	}
}

// --- admin commands ---

func runAdminCommand(t *testing.T, lab *jobLab, command string, args ...string) (string, string, *wire.ErrorMsg, int) {
	t.Helper()
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request(command, args...)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	stdout, stderr, errMsg, code := p.collect()
	return stdout, stderr, errMsg, code
}

func TestJobsAdminCommands(t *testing.T) {
	lab := newJobLab(t, func(_ context.Context, _ app.Command, rio RunIO) error {
		fmt.Fprint(rio.Stdout, "done\n")
		return nil
	})
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("stop")
	req.JobID = jobID(1)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	p.collectJob()

	stdout, _, errMsg, code := runAdminCommand(t, lab, "jobs-ls")
	if errMsg != nil || code != 0 || !strings.Contains(stdout, req.JobID) || !strings.Contains(stdout, "succeeded") || !strings.HasPrefix(stdout, "ID") {
		t.Fatalf("jobs ls: %q %v %d", stdout, errMsg, code)
	}
	stdout, _, _, code = runAdminCommand(t, lab, "jobs-ls", "--all")
	if code != 0 || !strings.Contains(stdout, "lists") {
		t.Fatalf("jobs ls --all: %q", stdout)
	}
	stdout, _, errMsg, code = runAdminCommand(t, lab, "jobs-show", req.JobID)
	var shown jobs.Status
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil || shown.ID != req.JobID || shown.State != jobs.StateSucceeded || errMsg != nil || code != 0 {
		t.Fatalf("jobs show: %q %v", stdout, err)
	}
	if strings.Contains(stdout, "secret") || strings.Contains(stdout, "env") {
		t.Fatalf("jobs show leaks request data: %s", stdout)
	}
	_, _, errMsg, code = runAdminCommand(t, lab, "jobs-show", jobID(15))
	if errMsg == nil || errMsg.Code != wire.CodeJobNotFound || code != 1 {
		t.Fatalf("unknown job: %+v %d", errMsg, code)
	}
	// Attach replays the whole output and returns the job's status.
	pa := startAgent(t, lab.options())
	pa.handshake("")
	attach := lab.request("jobs-attach", req.JobID)
	_ = pa.conn.WriteJSON(wire.TypeRequest, attach)
	out, _, _, exit, _ := pa.collectJob()
	if out != "done\n" || exit.Code != 0 {
		t.Fatalf("attach: %q %+v", out, exit)
	}
	stdout, _, _, code = runAdminCommand(t, lab, "jobs-cancel", req.JobID)
	if code != 0 || !strings.Contains(stdout, "is succeeded") {
		t.Fatalf("cancel of a finished job: %q %d", stdout, code)
	}
	stdout, _, _, code = runAdminCommand(t, lab, "jobs-prune")
	if code != 0 || !strings.Contains(stdout, "removed 0 finished job(s)") {
		t.Fatalf("prune: %q", stdout)
	}
}

func TestJobsCancelStopsARunningJobAndWaits(t *testing.T) {
	started := make(chan struct{})
	lab := newJobLab(t, func(ctx context.Context, _ app.Command, _ RunIO) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	p := startAgent(t, lab.options())
	p.handshake("")
	req := lab.request("up")
	req.JobID = jobID(2)
	_ = p.conn.WriteJSON(wire.TypeRequest, req)
	<-started
	stdout, _, errMsg, code := runAdminCommand(t, lab, "jobs-cancel", req.JobID)
	if errMsg != nil || code != 0 || !strings.Contains(stdout, "is cancelled") {
		t.Fatalf("cancel: %q %v %d", stdout, errMsg, code)
	}
	p.collectJob()
}

func TestTemplatesListAndPrune(t *testing.T) {
	lab := newJobLab(t, nil)
	store := blob.NewStore(lab.wd)
	old := qcow2Image("old and unused")
	oldSum := sha256.Sum256(old)
	oldDigest := hex.EncodeToString(oldSum[:])
	slot, err := store.Acquire(context.Background(), oldDigest, int64(len(old)))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := slot.Open()
	_ = u.Write(0, old)
	if err := u.Finish(); err != nil {
		t.Fatal(err)
	}
	slot.Release()
	past := time.Now().Add(-72 * time.Hour)
	_ = os.Chtimes(store.Path(oldDigest), past, past)

	stdout, _, errMsg, code := runAdminCommand(t, lab, "templates-ls")
	if errMsg != nil || code != 0 || !strings.Contains(stdout, oldDigest) || !strings.Contains(stdout, "REFERENCED-BY") {
		t.Fatalf("templates ls: %q %v", stdout, errMsg)
	}
	stdout, _, _, code = runAdminCommand(t, lab, "templates-prune", "--min-age", "100h")
	if code != 0 || !strings.Contains(stdout, "nothing to remove") {
		t.Fatalf("young prune: %q", stdout)
	}
	stdout, _, _, code = runAdminCommand(t, lab, "templates-prune")
	if code != 0 || !strings.Contains(stdout, "removed "+oldDigest) {
		t.Fatalf("prune: %q", stdout)
	}
	if _, err := os.Stat(store.Path(oldDigest)); !os.IsNotExist(err) {
		t.Fatalf("pruned blob remains: %v", err)
	}
	_, _, errMsg, code = runAdminCommand(t, lab, "templates-prune", "--max-age", "1h")
	if errMsg == nil || errMsg.Code != wire.CodeProtocolError || code != 1 {
		t.Fatalf("bad flag: %+v %d", errMsg, code)
	}
	if _, err := filepath.Abs(lab.wd); err != nil {
		t.Fatal(err)
	}
}
