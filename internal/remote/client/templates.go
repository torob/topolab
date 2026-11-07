package client

import (
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/torob/topolab/internal/remote/wire"
)

// prepareTemplates hashes the templates that VMs use and fills inv.Templates.
// It returns the source file of each digest.
func (r *Runner) prepareTemplates(inv *Invocation) (map[string]string, error) {
	if inv.Topology == nil {
		return nil, nil
	}
	files := map[string]string{}
	seen := map[string]bool{}
	var refs []wire.TemplateRef
	for _, vm := range inv.Topology.VMs {
		if seen[vm.Template] {
			continue
		}
		seen[vm.Template] = true
		tmpl, ok := inv.Topology.LookupTemplate(vm.Template)
		if !ok {
			return nil, fmt.Errorf("VM %q uses unknown template %q", vm.Name, vm.Template)
		}
		digest, size, err := r.DigestCache.Digest(tmpl.Path, inv.Stderr)
		if err != nil {
			return nil, fmt.Errorf("template %q: %w", tmpl.Name, err)
		}
		refs = append(refs, wire.TemplateRef{Name: tmpl.Name, SHA256: digest, Size: size})
		files[digest] = tmpl.Path
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	inv.Templates = refs
	return files, nil
}

// forgetTemplates drops the cached digests of an invocation's templates.
func (r *Runner) forgetTemplates(inv Invocation, files map[string]string) {
	for _, path := range files {
		r.DigestCache.Forget(path)
	}
}

// transfer is the upload of one template.
type transfer struct {
	digest      string
	f           *os.File
	size        int64
	next, acked int64
	inflight    int
	ended       bool
	startOffset int64
	started     time.Time
}

// uploader sends the templates that the agent lacks, keeping at most
// wire.BlobWindow chunks in flight (spec.md Section 12.5). Its methods run on
// the frame loop's goroutine.
type uploader struct {
	files    map[string]string
	sizes    map[string]int64
	queue    []string
	cur      *transfer
	progress io.Writer
	lastShow time.Time
	failed   bool
	// offsetsPending are the resume offsets from the agent's status frame.
	offsetsPending map[string]int64
}

func newUploader(refs []wire.TemplateRef, files map[string]string, progress io.Writer) *uploader {
	sizes := map[string]int64{}
	for _, r := range refs {
		sizes[r.SHA256] = r.Size
	}
	return &uploader{files: files, sizes: sizes, progress: progress}
}

// handle processes one blob frame.
func (u *uploader) handle(s *session, f wire.Frame) error {
	switch f.Type {
	case wire.TypeBlobStatus:
		var st wire.BlobStatus
		if err := f.JSON(&st); err != nil {
			return err
		}
		return u.status(s, st)
	case wire.TypeBlobAck:
		var a wire.BlobAck
		if err := f.JSON(&a); err != nil {
			return err
		}
		return u.ack(s, a)
	case wire.TypeBlobDone:
		var d wire.BlobDone
		if err := f.JSON(&d); err != nil {
			return err
		}
		return u.done(s, d)
	}
	return nil
}

func (u *uploader) status(s *session, st wire.BlobStatus) error {
	offsets := map[string]int64{}
	for _, b := range st.Blobs {
		if _, ok := u.sizes[b.SHA256]; !ok {
			return fmt.Errorf("%w: agent reported unknown digest %s", wire.ErrProtocol, b.SHA256)
		}
		if b.State == wire.BlobPresent {
			continue
		}
		u.queue = append(u.queue, b.SHA256)
		offsets[b.SHA256] = b.Offset
	}
	sort.Strings(u.queue)
	u.offsetsPending = offsets
	return u.begin(s, offsets)
}

// begin starts the next transfer, if any.
func (u *uploader) begin(s *session, offsets map[string]int64) error {
	if len(u.queue) == 0 {
		u.cur = nil
		return nil
	}
	digest := u.queue[0]
	u.queue = u.queue[1:]
	path, ok := u.files[digest]
	if !ok {
		return fmt.Errorf("no source file for digest %s", digest)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	size := u.sizes[digest]
	offset := offsets[digest]
	if offset > size {
		offset = 0
	}
	u.cur = &transfer{digest: digest, f: f, size: size, next: offset, acked: offset, startOffset: offset, started: time.Now()}
	return u.fill(s)
}

// fill sends chunks while the window has room, then the end marker.
func (u *uploader) fill(s *session) error {
	t := u.cur
	for t != nil && t.inflight < wire.BlobWindow && t.next < t.size {
		n := int64(wire.BlobChunk)
		if t.size-t.next < n {
			n = t.size - t.next
		}
		buf := make([]byte, n)
		if _, err := t.f.ReadAt(buf, t.next); err != nil {
			return fmt.Errorf("read %s: %w", u.files[t.digest], err)
		}
		payload, err := wire.EncodeBlobData(t.digest, t.next, buf)
		if err != nil {
			return err
		}
		if err := s.send(wire.TypeBlobData, payload); err != nil {
			return err
		}
		t.next += n
		t.inflight++
	}
	if t != nil && t.next >= t.size && !t.ended {
		t.ended = true
		return s.sendJSON(wire.TypeBlobEnd, wire.BlobEnd{SHA256: t.digest, Size: t.size})
	}
	return nil
}

func (u *uploader) ack(s *session, a wire.BlobAck) error {
	t := u.cur
	if t == nil || a.SHA256 != t.digest {
		return nil
	}
	t.acked = a.Offset
	if t.inflight > 0 {
		t.inflight--
	}
	u.show(t, false)
	return u.fill(s)
}

func (u *uploader) done(s *session, d wire.BlobDone) error {
	t := u.cur
	if t == nil || d.SHA256 != t.digest {
		return nil
	}
	_ = t.f.Close()
	if !d.OK {
		// The agent follows with an error frame and an exit frame.
		u.failed = true
		u.cur = nil
		return nil
	}
	u.show(t, true)
	return u.begin(s, u.offsetsPending)
}

// show prints progress at most once a second, and once at the end.
func (u *uploader) show(t *transfer, final bool) {
	if u.progress == nil {
		return
	}
	now := time.Now()
	if !final && now.Sub(u.lastShow) < time.Second {
		return
	}
	u.lastShow = now
	sent := t.acked
	if final {
		sent = t.size
	}
	rate := ""
	if elapsed := now.Sub(t.started).Seconds(); elapsed > 0 {
		rate = fmt.Sprintf(" (%.1f MiB/s)", float64(sent-t.startOffset)/elapsed/(1<<20))
	}
	pct := 100.0
	if t.size > 0 {
		pct = float64(sent) / float64(t.size) * 100
	}
	fmt.Fprintf(u.progress, "topolab: uploading template %s: %.0f%% of %d MiB%s\n", t.digest[:12], pct, t.size>>20, rate)
}

// close releases the open source file after an aborted session.
func (u *uploader) close() {
	if u.cur != nil && u.cur.f != nil {
		_ = u.cur.f.Close()
	}
}
