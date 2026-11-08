package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/blob"
	"github.com/torob/topolab/internal/remote/wire"
)

// usedTemplates returns the references of the templates that VMs use and that
// the controller uploads, by name. A template with remote: true has no
// reference.
func usedTemplates(p *parsed, req wire.Request) ([]wire.TemplateRef, error) {
	byName := map[string]wire.TemplateRef{}
	for _, t := range req.Templates {
		byName[t.Name] = t
	}
	seen := map[string]bool{}
	var out []wire.TemplateRef
	for _, vm := range p.cfg.VMs {
		if seen[vm.Template] {
			continue
		}
		seen[vm.Template] = true
		if t, ok := p.cfg.LookupTemplate(vm.Template); ok && t.Remote {
			continue
		}
		ref, ok := byName[vm.Template]
		if !ok {
			return nil, reqErr(wire.CodeProtocolError, "template %q is used by VM %q but the request does not list it", vm.Template, vm.Name)
		}
		out = append(out, ref)
	}
	return out, nil
}

// checkHostTemplates validates each template with remote: true that a VM uses,
// before any job exists (spec.md Section 12.5).
func checkHostTemplates(p *parsed) error {
	seen := map[string]bool{}
	var used []*config.Template
	for _, vm := range p.cfg.VMs {
		if seen[vm.Template] {
			continue
		}
		seen[vm.Template] = true
		if t, ok := p.cfg.LookupTemplate(vm.Template); ok && t.Remote {
			used = append(used, t)
		}
	}
	if err := config.ValidateTemplateFiles(used); err != nil {
		return reqErr(wire.CodeHostTemplate, "%v", err)
	}
	return nil
}

// applyTemplates points each listed template at its blob and returns the
// digest that every blob path must have.
func applyTemplates(p *parsed, req wire.Request, store *blob.Store) (map[string]string, error) {
	digests := map[string]string{}
	byName := map[string]wire.TemplateRef{}
	for _, t := range req.Templates {
		byName[t.Name] = t
	}
	for _, t := range p.cfg.Templates {
		ref, ok := byName[t.Name]
		if !ok {
			continue
		}
		t.Path = store.Path(ref.SHA256)
		digests[t.Path] = ref.SHA256
	}
	return digests, nil
}

// blobCode maps a blob error to its protocol error code.
func blobCode(err error) string {
	switch {
	case errors.Is(err, blob.ErrDigestMismatch):
		return wire.CodeBlobDigest
	case errors.Is(err, blob.ErrInvalid):
		return wire.CodeBlobInvalid
	case errors.Is(err, blob.ErrNoSpace):
		return wire.CodeBlobNoSpace
	case errors.Is(err, blob.ErrCorrupt):
		return wire.CodeBlobCorrupt
	}
	return wire.CodeProtocolError
}

// blobFrames queues blob frames from the read loop. The controller keeps at
// most wire.BlobWindow chunks in flight, so the queue never fills.
type blobFrames chan wire.Frame

// receiveTemplates makes every template that a VM uses present in the blob
// store, receiving the missing bytes from the controller. It returns a release
// function; the caller calls it once the job is registered, because until then
// `.gc.lock` must stay held in shared mode.
func (s *session) receiveTemplates(ctx context.Context, p *parsed, req wire.Request) (release func(), err error) {
	store := blob.NewStore(p.workingDir)
	refs, err := usedTemplates(p, req)
	if err != nil {
		return nil, err
	}
	gc, err := store.GCShared(ctx)
	if err != nil {
		return nil, err
	}
	var slots []*blob.Slot
	cleanup := func() {
		for _, sl := range slots {
			sl.Release()
		}
		slots = nil
		_ = gc.Close()
	}
	// A failure releases everything; `return nil, err` clears the named
	// results, so the deferred check must not call them.
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	sizes := map[string]int64{}
	for _, r := range refs {
		if prior, ok := sizes[r.SHA256]; ok && prior != r.Size {
			return nil, reqErr(wire.CodeProtocolError, "digest %s is listed with two sizes", r.SHA256)
		}
		sizes[r.SHA256] = r.Size
	}
	digests := make([]string, 0, len(sizes))
	for d := range sizes {
		digests = append(digests, d)
	}
	sort.Strings(digests) // every uploader locks in the same order

	var status wire.BlobStatus
	var pending []*blob.Slot
	for _, d := range digests {
		slot, err := store.Acquire(ctx, d, sizes[d])
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, reqErr(blobCode(err), "%v", err)
		}
		slots = append(slots, slot)
		status.Blobs = append(status.Blobs, wire.BlobState{SHA256: d, State: slot.State, Offset: slot.Offset})
		if slot.State == blob.StatePresent {
			store.Touch(d, s.opts.Now())
			continue
		}
		pending = append(pending, slot)
	}

	frames := make(blobFrames, 4*wire.BlobWindow)
	s.setHandler(func(f wire.Frame) {
		switch f.Type {
		case wire.TypeBlobData, wire.TypeBlobEnd:
			select {
			case frames <- f:
			default:
				s.cancel(fmt.Errorf("%w: blob frames overflowed the window", wire.ErrProtocol))
			}
		}
	})
	defer s.setHandler(nil)
	if err := s.conn.WriteJSON(wire.TypeBlobStatus, status); err != nil {
		return nil, err
	}
	for _, slot := range pending {
		if err := s.receiveBlob(ctx, slot, frames); err != nil {
			return nil, err
		}
		store.Touch(slot.Digest, s.opts.Now())
	}
	return cleanup, nil
}

// receiveBlob receives one blob from the queued frames.
func (s *session) receiveBlob(ctx context.Context, slot *blob.Slot, frames blobFrames) error {
	fail := func(err error) error {
		code := blobCode(err)
		_ = s.conn.WriteJSON(wire.TypeBlobDone, wire.BlobDone{SHA256: slot.Digest, OK: false, Code: code, Message: err.Error()})
		return reqErr(code, "%v", err)
	}
	upload, err := slot.Open()
	if err != nil {
		return fail(err)
	}
	defer upload.Abort()
	for {
		var f wire.Frame
		select {
		case f = <-frames:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		switch f.Type {
		case wire.TypeBlobData:
			digest, offset, data, err := wire.DecodeBlobData(f.Payload)
			if err != nil {
				return reqErr(wire.CodeProtocolError, "%v", err)
			}
			if digest != slot.Digest {
				return reqErr(wire.CodeProtocolError, "data for digest %s while %s is being received", digest, slot.Digest)
			}
			if err := upload.Write(offset, data); err != nil {
				return fail(err)
			}
			if err := s.conn.WriteJSON(wire.TypeBlobAck, wire.BlobAck{SHA256: digest, Offset: upload.Offset()}); err != nil {
				return err
			}
		case wire.TypeBlobEnd:
			var end wire.BlobEnd
			if err := f.JSON(&end); err != nil {
				return err
			}
			if end.SHA256 != slot.Digest {
				return reqErr(wire.CodeProtocolError, "end of digest %s while %s is being received", end.SHA256, slot.Digest)
			}
			if err := upload.Finish(); err != nil {
				return fail(err)
			}
			return s.conn.WriteJSON(wire.TypeBlobDone, wire.BlobDone{SHA256: slot.Digest, OK: true})
		}
	}
}
