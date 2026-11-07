package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/agent"
	"github.com/torob/topolab/internal/remote/jobs"
)

// liveProcs stands in for /proc: every runner looks alive, as the test process
// plays the runner.
type liveProcs struct{ jobs.OSProcs }

func (liveProcs) Cmdline(int) ([]string, error) { return []string{"topolab", "__job"}, nil }

// lab runs the agent and its job runners in this process.
type lab struct {
	t       *testing.T
	wd      string
	runner  *fakeRunner
	mu      sync.Mutex
	cancels []context.CancelFunc
	wg      sync.WaitGroup
	spawned atomic.Int32
	dials   atomic.Int32
	// current is the transport of the latest connection, so tests can cut it.
	current atomic.Pointer[Transport]
	// failDials makes every dial fail like an unreachable host.
	failDials atomic.Bool
	written   atomic.Int64
	// execRunner, when set, runs exec commands instead of runner.
	execRunner agent.Runner
}

// Run dispatches exec commands to the shell runner and everything else to the
// fake runner.
func (l *lab) Run(ctx context.Context, command app.Command, rio agent.RunIO) error {
	if rio.ExecHost != nil && l.execRunner != nil {
		return l.execRunner.Run(ctx, command, rio)
	}
	return l.runner.Run(ctx, command, rio)
}

func newLab(t *testing.T, run func(ctx context.Context, command app.Command, rio agent.RunIO) error) *lab {
	t.Helper()
	wd, err := os.MkdirTemp("/tmp", "tlcl-")
	if err != nil {
		t.Fatal(err)
	}
	l := &lab{t: t, wd: wd, runner: &fakeRunner{run: run}}
	t.Cleanup(func() {
		l.cancelAll()
		l.wg.Wait()
		os.RemoveAll(wd)
	})
	return l
}

func (l *lab) cancelAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.cancels {
		c()
	}
}

func (l *lab) options() agent.Options {
	return agent.Options{
		Digest:        testDigest,
		Runner:        l,
		ExecKillAfter: 200 * time.Millisecond,
		JobProcs:      liveProcs{},
		PollInterval:  5 * time.Millisecond,
		SpawnJob: func(dir string) error {
			ctx, cancel := context.WithCancel(context.Background())
			l.mu.Lock()
			l.cancels = append(l.cancels, cancel)
			l.mu.Unlock()
			l.spawned.Add(1)
			l.wg.Add(1)
			go func() { defer l.wg.Done(); agent.RunJob(ctx, dir, l) }()
			return nil
		},
		SignalRunner: func(int) { l.cancelAll() },
	}
}

type countingWriteCloser struct {
	io.WriteCloser
	n *atomic.Int64
}

func (c countingWriteCloser) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.WriteCloser.Write(p)
}

// Dial implements Dialer.
func (l *lab) Dial(ctx context.Context, inv Invocation) (*Transport, error) {
	l.dials.Add(1)
	if l.failDials.Load() {
		return &Transport{In: strings.NewReader(""), Out: nopWriteCloser{io.Discard}, Wait: func() error { return exitCode(255) }, Kill: func() {}}, nil
	}
	clientRead, agentWrite := io.Pipe()
	agentRead, clientWrite := io.Pipe()
	agentCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := agent.Serve(agentCtx, agentRead, agentWrite, l.options())
		_ = agentWrite.Close()
		done <- err
	}()
	tr := &Transport{
		In:   clientRead,
		Out:  countingWriteCloser{WriteCloser: clientWrite, n: &l.written},
		Wait: func() error { <-done; return nil },
		Kill: func() { cancel(); _ = agentWrite.Close(); _ = clientWrite.Close(); _ = clientRead.Close() },
	}
	l.current.Store(tr)
	return tr, nil
}

func (l *lab) runner0(sigs chan os.Signal) *Runner {
	r := newRunner(l, sigs)
	r.BackoffInitial = 5 * time.Millisecond
	r.BackoffMax = 20 * time.Millisecond
	return r
}

func (l *lab) invocation(command string, args ...string) (Invocation, *syncBuffer, *syncBuffer) {
	var stdout, stderr syncBuffer
	cfg, _, err := config.Parse([]byte(testTopology), l.t.TempDir(), nil)
	if err != nil {
		l.t.Fatal(err)
	}
	return Invocation{
		Destination: "ops@lab", Command: command, Args: args, Topology: cfg, TopologyData: []byte(testTopology),
		BaseDir: l.t.TempDir(), WorkingDir: l.wd, ReconnectTimeout: 2 * time.Second, Stdout: &stdout, Stderr: &stderr,
	}, &stdout, &stderr
}

// syncBuffer is a bytes.Buffer that tests can read while the client writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestLifecycleCommandRunsAsAJob(t *testing.T) {
	l := newLab(t, func(_ context.Context, command app.Command, rio agent.RunIO) error {
		fmt.Fprintf(rio.Stdout, "%s done\n", command.Name)
		return nil
	})
	inv, stdout, stderr := l.invocation("stop")
	code := l.runner0(nil).Run(context.Background(), inv)
	if code != 0 || stdout.String() != "stop done\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	list, err := jobs.NewStore(l.wd).List("lists")
	if err != nil || len(list) != 1 || len(list[0].ID) != 32 || list[0].Command != "stop" {
		t.Fatalf("jobs = %+v %v", list, err)
	}
	if l.dials.Load() != 1 || l.spawned.Load() != 1 {
		t.Fatalf("dials=%d spawned=%d", l.dials.Load(), l.spawned.Load())
	}
}

func TestFailedJobExitsWithItsStatusAndMessage(t *testing.T) {
	l := newLab(t, func(context.Context, app.Command, agent.RunIO) error { return errors.New("network is absent") })
	inv, _, stderr := l.invocation("start")
	if code := l.runner0(nil).Run(context.Background(), inv); code != 1 || stderr.String() != "topolab: network is absent\n" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestLostConnectionIsRetriedAndOutputIsNeitherLostNorRepeated(t *testing.T) {
	release := make(chan struct{})
	l := newLab(t, func(_ context.Context, _ app.Command, rio agent.RunIO) error {
		fmt.Fprint(rio.Stdout, "one\n")
		<-release
		fmt.Fprint(rio.Stdout, "two\n")
		return nil
	})
	inv, stdout, stderr := l.invocation("up")
	done := make(chan int, 1)
	go func() { done <- l.runner0(nil).Run(context.Background(), inv) }()
	waitFor(t, "the first output", func() bool { return stdout.String() == "one\n" })
	l.current.Load().Kill() // the connection drops while the job runs
	waitFor(t, "a second connection", func() bool { return l.dials.Load() >= 2 })
	close(release)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("run did not finish; stderr=%q", stderr)
	}
	if stdout.String() != "one\ntwo\n" {
		t.Fatalf("stdout = %q, want each line once", stdout)
	}
	if l.spawned.Load() != 1 {
		t.Fatalf("a reconnect started %d runners, want 1", l.spawned.Load())
	}
	if !strings.Contains(stderr.String(), "connection to the agent was lost") || !strings.Contains(stderr.String(), "reconnecting in") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestReconnectTimeoutDetachesWithStatusThreeAndLeavesTheJob(t *testing.T) {
	release := make(chan struct{})
	l := newLab(t, func(_ context.Context, _ app.Command, rio agent.RunIO) error {
		fmt.Fprint(rio.Stdout, "started\n")
		<-release
		return nil
	})
	defer close(release)
	inv, stdout, stderr := l.invocation("up")
	inv.ReconnectTimeout = 150 * time.Millisecond
	done := make(chan int, 1)
	go func() { done <- l.runner0(nil).Run(context.Background(), inv) }()
	waitFor(t, "the first output", func() bool { return stdout.String() == "started\n" })
	l.failDials.Store(true)
	l.current.Load().Kill()
	select {
	case code := <-done:
		if code != ExitDetached {
			t.Fatalf("code = %d, want %d; stderr=%q", code, ExitDetached, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no exit after the reconnect timeout; stderr=%q", stderr)
	}
	list, _ := jobs.NewStore(l.wd).List("lists")
	if len(list) != 1 || jobs.Terminal(list[0].State) {
		t.Fatalf("the job did not survive: %+v", list)
	}
	want := "topolab jobs attach " + list[0].ID
	if !strings.Contains(stderr.String(), "detached; job "+list[0].ID+" keeps running on ops@lab") || !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestZeroReconnectTimeoutDisablesReconnection(t *testing.T) {
	release := make(chan struct{})
	l := newLab(t, func(_ context.Context, _ app.Command, rio agent.RunIO) error {
		fmt.Fprint(rio.Stdout, "started\n")
		<-release
		return nil
	})
	defer close(release)
	inv, stdout, _ := l.invocation("up")
	inv.ReconnectTimeout = 0
	done := make(chan int, 1)
	go func() { done <- l.runner0(nil).Run(context.Background(), inv) }()
	waitFor(t, "the first output", func() bool { return stdout.String() == "started\n" })
	l.current.Load().Kill()
	select {
	case code := <-done:
		if code != ExitDetached || l.dials.Load() != 1 {
			t.Fatalf("code=%d dials=%d", code, l.dials.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit")
	}
}

func TestFirstConnectionFailureIsNotRetried(t *testing.T) {
	l := newLab(t, nil)
	l.failDials.Store(true)
	inv, _, stderr := l.invocation("up")
	if code := l.runner0(nil).Run(context.Background(), inv); code != 1 || l.dials.Load() != 1 {
		t.Fatalf("code=%d dials=%d stderr=%q", code, l.dials.Load(), stderr)
	}
}

func TestFirstInterruptCancelsTheJobAndKeepsTheControllerAttached(t *testing.T) {
	started := make(chan struct{})
	l := newLab(t, func(ctx context.Context, _ app.Command, _ agent.RunIO) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	sigs := make(chan os.Signal, 4)
	inv, _, stderr := l.invocation("down")
	done := make(chan int, 1)
	go func() { done <- l.runner0(sigs).Run(context.Background(), inv) }()
	<-started
	sigs <- os.Interrupt
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("code = %d; stderr=%q", code, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("a cancelled job did not end the run; stderr=%q", stderr)
	}
	list, _ := jobs.NewStore(l.wd).List("lists")
	waitFor(t, "the cancelled state", func() bool {
		list, _ = jobs.NewStore(l.wd).List("lists")
		return len(list) == 1 && list[0].State == jobs.StateCancelled
	})
}

func TestSecondInterruptAndTermDetachWithoutCancelling(t *testing.T) {
	for name, send := range map[string][]os.Signal{
		"second interrupt": {os.Interrupt, os.Interrupt},
		"terminate":        {syscall.SIGTERM},
		"hangup":           {syscall.SIGHUP},
	} {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			l := newLab(t, func(context.Context, app.Command, agent.RunIO) error {
				close(started)
				<-release // ignores cancellation
				return nil
			})
			defer close(release)
			sigs := make(chan os.Signal, 4)
			inv, _, stderr := l.invocation("up")
			done := make(chan int, 1)
			go func() { done <- l.runner0(sigs).Run(context.Background(), inv) }()
			<-started
			for _, s := range send {
				sigs <- s
			}
			select {
			case code := <-done:
				if code != ExitDetached || !strings.Contains(stderr.String(), "detached; job ") {
					t.Fatalf("code=%d stderr=%q", code, stderr)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("did not detach; stderr=%q", stderr)
			}
			list, _ := jobs.NewStore(l.wd).List("lists")
			if len(list) != 1 || jobs.Terminal(list[0].State) {
				t.Fatalf("a detach ended the job: %+v", list)
			}
		})
	}
}

func TestSecondLifecycleCommandFailsWhileAJobRuns(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	l := newLab(t, func(context.Context, app.Command, agent.RunIO) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	})
	defer close(release)
	sigs := make(chan os.Signal, 4)
	first, _, _ := l.invocation("up")
	go l.runner0(sigs).Run(context.Background(), first)
	<-started
	second, _, stderr := l.invocation("down")
	if code := l.runner0(nil).Run(context.Background(), second); code != 1 || !strings.Contains(stderr.String(), "remote: job_active:") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	sigs <- syscall.SIGTERM // let the first run return
}

func TestJobsAttachFollowsAnExistingJob(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	l := newLab(t, func(_ context.Context, _ app.Command, rio agent.RunIO) error {
		fmt.Fprint(rio.Stdout, "early\n")
		close(started)
		<-release
		fmt.Fprint(rio.Stdout, "late\n")
		return nil
	})
	sigs := make(chan os.Signal, 4)
	first, _, _ := l.invocation("up")
	go l.runner0(sigs).Run(context.Background(), first)
	<-started
	waitFor(t, "the job record", func() bool {
		list, _ := jobs.NewStore(l.wd).List("lists")
		return len(list) == 1
	})
	list, _ := jobs.NewStore(l.wd).List("lists")
	attach, stdout, stderr := l.invocation("jobs-attach", list[0].ID)
	done := make(chan int, 1)
	go func() { done <- l.runner0(nil).Run(context.Background(), attach) }()
	waitFor(t, "replayed output", func() bool { return stdout.String() == "early\n" })
	close(release)
	if code := <-done; code != 0 || stdout.String() != "early\nlate\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	sigs <- syscall.SIGTERM
	unknown, _, stderr2 := l.invocation("jobs-attach", strings.Repeat("0", 32))
	if code := l.runner0(nil).Run(context.Background(), unknown); code != 1 || !strings.Contains(stderr2.String(), "job_not_found") {
		t.Fatalf("unknown job: code=%d stderr=%q", code, stderr2)
	}
}

// --- templates ---

const vmTopology = `name: lab
templates:
  - {name: base, path: base.qcow2}
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

func writeTemplate(t *testing.T, dir string, size int) string {
	t.Helper()
	data := make([]byte, 24, size)
	copy(data, "QFI\xfb")
	binary.BigEndian.PutUint32(data[4:8], 3)
	for len(data) < size {
		data = append(data, byte(len(data)*7))
	}
	path := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func (l *lab) upInvocation(t *testing.T, size int) (Invocation, *syncBuffer, *syncBuffer, string) {
	t.Helper()
	dir := t.TempDir()
	path := writeTemplate(t, dir, size)
	topo := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(topo, []byte(vmTopology), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.ParseWith([]byte(vmTopology), config.ParseOptions{BaseDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr syncBuffer
	return Invocation{
		Destination: "ops@lab", Command: "up", Topology: cfg, TopologyData: []byte(vmTopology), BaseDir: dir,
		WorkingDir: l.wd, ReconnectTimeout: 2 * time.Second, Stdout: &stdout, Stderr: &stderr,
	}, &stdout, &stderr, path
}

func TestUpUploadsTheTemplateOnceAndLaterRunsSendNothing(t *testing.T) {
	var gotPath string
	var digests map[string]string
	l := newLab(t, func(_ context.Context, command app.Command, rio agent.RunIO) error {
		gotPath, digests = command.Topology.Templates[0].Path, rio.TemplateDigests
		return nil
	})
	const size = 3<<20 + 123
	inv, _, stderr, path := l.upInvocation(t, size)
	r := l.runner0(nil)
	r.DigestCache = &DigestCache{Path: filepath.Join(t.TempDir(), "cache", "digests.json")}
	if code := r.Run(context.Background(), inv); code != 0 {
		t.Fatalf("first up: code=%d stderr=%q", code, stderr)
	}
	firstBytes := l.written.Load()
	if firstBytes < size {
		t.Fatalf("the first up sent %d bytes, less than the %d byte template", firstBytes, size)
	}
	if !strings.HasPrefix(gotPath, filepath.Join(l.wd, ".topolab", "blobs", "sha256")) || digests[gotPath] == "" {
		t.Fatalf("template path = %q digests = %v", gotPath, digests)
	}
	onDisk, _ := os.ReadFile(gotPath)
	source, _ := os.ReadFile(path)
	if !bytes.Equal(onDisk, source) {
		t.Fatal("the uploaded blob differs from the source file")
	}
	if !strings.Contains(stderr.String(), "uploading template") {
		t.Fatalf("no upload progress: %q", stderr)
	}

	// Wait for the first job to end, then run again: nothing is uploaded.
	waitFor(t, "the first job to end", func() bool {
		list, _ := jobs.NewStore(l.wd).List("lab")
		return len(list) == 1 && jobs.Terminal(list[0].State)
	})
	l.written.Store(0)
	inv2, _, stderr2, _ := l.upInvocation(t, size)
	if code := l.runner0(nil).Run(context.Background(), inv2); code != 0 {
		t.Fatalf("second up: code=%d stderr=%q", code, stderr2)
	}
	if sent := l.written.Load(); sent > 64<<10 {
		t.Fatalf("the second up sent %d bytes; the template must not be sent again", sent)
	}
	if strings.Contains(stderr2.String(), "uploading template") {
		t.Fatalf("second up uploaded: %q", stderr2)
	}
}

func TestDigestMismatchIsRetriedOnceFromAFreshHash(t *testing.T) {
	l := newLab(t, nil)
	inv, _, stderr, path := l.upInvocation(t, 400<<10)
	cache := &DigestCache{Path: filepath.Join(t.TempDir(), "digests.json")}
	// Poison the cache: right key, wrong digest.
	if _, _, err := cache.Digest(path, nil); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	for k, e := range cache.entries {
		e.Digest = strings.Repeat("f", 64)
		cache.entries[k] = e
	}
	cache.mu.Unlock()
	r := l.runner0(nil)
	r.DigestCache = cache
	if code := r.Run(context.Background(), inv); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr.String(), "blob_digest_mismatch") || !strings.Contains(stderr.String(), "hashing it again") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestBlobFailuresReachTheOperator(t *testing.T) {
	l := newLab(t, nil)
	inv, _, stderr, path := l.upInvocation(t, 100<<10)
	// Replace the template with a file that is not a qcow2 image after the
	// controller validated it: the agent must refuse it.
	if err := os.WriteFile(path, bytes.Repeat([]byte("not an image "), 10000), 0o644); err != nil {
		t.Fatal(err)
	}
	code := l.runner0(nil).Run(context.Background(), inv)
	if code != 1 || !strings.Contains(stderr.String(), "remote: blob_invalid:") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if list, _ := jobs.NewStore(l.wd).List("lab"); len(list) != 0 {
		t.Fatalf("a rejected upload created a job: %+v", list)
	}
}

func TestDigestCacheServesUnchangedFilesAndNotchangedOnes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := &DigestCache{Path: filepath.Join(dir, "cache", "digests.json")}
	digest, size, err := cache.Digest(path, nil)
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if err != nil || digest != want || size != 3 {
		t.Fatalf("digest = %s %d %v", digest, size, err)
	}
	if fi, err := os.Stat(cache.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(cache.Path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir: %v %v", fi, err)
	}
	// Tamper with the stored value: an unchanged file is served from the cache.
	cache.mu.Lock()
	for k, e := range cache.entries {
		e.Digest = "cached"
		cache.entries[k] = e
	}
	cache.mu.Unlock()
	if got, _, _ := cache.Digest(path, nil); got != "cached" {
		t.Fatalf("unchanged file was rehashed: %s", got)
	}
	// A fresh cache object reads the persisted file too.
	cache.save()
	fresh := &DigestCache{Path: cache.Path}
	if got, _, _ := fresh.Digest(path, nil); got != "cached" {
		t.Fatalf("persisted cache not used: %s", got)
	}
	// Any change to the file invalidates the entry.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := cache.Digest(path, nil); got != want {
		t.Fatalf("changed file was served from the cache: %s", got)
	}
	cache.Forget(path)
	cache.mu.Lock()
	n := len(cache.entries)
	cache.mu.Unlock()
	if n != 0 {
		t.Fatalf("Forget left %d entries", n)
	}
	// A nil cache and a cache without a path still hash.
	var none *DigestCache
	if got, _, err := none.Digest(path, nil); err != nil || got != want {
		t.Fatalf("nil cache: %s %v", got, err)
	}
	if _, _, err := cache.Digest(filepath.Join(dir, "missing"), nil); err == nil {
		t.Fatal("a missing file has a digest")
	}
}

func TestDefaultDigestCachePath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/xdg")
	if got := DefaultDigestCachePath(); got != "/xdg/topolab/digests.json" {
		t.Fatalf("path = %q", got)
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/home/someone")
	if got := DefaultDigestCachePath(); got != "/home/someone/.cache/topolab/digests.json" {
		t.Fatalf("path = %q", got)
	}
}
