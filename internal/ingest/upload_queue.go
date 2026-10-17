package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// queueDeferredUpload records a metered-deferred upload in queue.db as an
// OriginUpload row, so Drain uploads the verified local copy once the link
// allows it -- without the card, and across a restart. This is invariant 3
// applied to upload mode: the intent is persisted before any network call
// (the upload itself is the only call, and it happens later, from the row).
//
// On success the file is Queued, not Deferred: nothing depends on the card
// any more, matching offline ingest. Without a queue, or when the insert
// fails (logged), the result is left Deferred exactly as before, so the
// card stays the only copy and the "re-insert it" notice still holds.
//
// Idempotent: a copy a pending row already claims (the same card file
// re-ingested while metered) reports Queued without a second row. A copy
// that is already in place with no row (a deferral from before this stage
// existed, or a finished row whose move was blocked) is hashed from the
// local edit copy -- never the card -- to build one.
func (e *Engine) queueDeferredUpload(ctx context.Context, fr FileResult, it uploadItem) FileResult {
	if e.Queue == nil || it.noQueue {
		return fr
	}
	// The row is the durable record: it is written even if the ingest is
	// being cancelled, like registerNode's insert.
	durable := context.WithoutCancel(ctx)

	if rec, found, err := e.Queue.PendingUploadByLocalPath(durable, fr.LocalPath); err != nil {
		slog.Warn("upload: could not look up a queued upload; leaving the file deferred", "path", fr.LocalPath, "err", err)
		return fr
	} else if found {
		return markUploadQueued(fr, rec.NodeUUID)
	}

	// Another row (an offline or online ingest) may own this copy, and a
	// drained upload would move it out from under that row. Fail closed: a
	// lookup error leaves the file deferred, like the lookup above.
	if other, found, err := e.Queue.ByLocalPath(durable, fr.LocalPath); err != nil {
		slog.Warn("upload: could not check whether another queue row owns the local copy; leaving the file deferred", "path", fr.LocalPath, "err", err)
		return fr
	} else if found && other.Origin != queue.OriginUpload {
		slog.Warn("upload: the local copy belongs to another queue row; leaving the file deferred", "path", fr.LocalPath, "node", other.NodeUUID)
		return fr
	}

	info, err := os.Stat(fr.LocalPath)
	if err != nil {
		slog.Warn("upload: could not stat the local copy to queue its upload; leaving the file deferred", "path", fr.LocalPath, "err", err)
		return fr
	}

	// A row Drain gave up on (FAILED) already owns this copy: queueing a
	// second row on the same path would leave the stale one in the failed
	// list for good, and once the new row's upload moves the copy the old
	// one's Retry could only fail again. Reuse it -- but only while the copy
	// still matches the size and mtime recorded for it, so the recorded
	// BLAKE3 still describes these bytes; otherwise the file stays Deferred.
	if failed, found, err := e.Queue.UnfinishedUploadByLocalPath(durable, fr.LocalPath); err != nil {
		slog.Warn("upload: could not look up a failed upload for this copy; leaving the file deferred", "path", fr.LocalPath, "err", err)
		return fr
	} else if found { // PENDING returned above, so this one is FAILED
		if info.Size() != failed.SizeBytes || info.ModTime().Unix() != failed.MtimeUnix {
			slog.Warn("upload: a failed upload row owns this copy but its size or mtime no longer matches; leaving the file deferred", "path", fr.LocalPath, "node", failed.NodeUUID)
			return fr
		}
		if _, err := e.Queue.ResetFailedNode(durable, failed.NodeUUID); err != nil {
			slog.Warn("upload: could not re-queue the failed upload for this copy; leaving the file deferred", "path", fr.LocalPath, "node", failed.NodeUUID, "err", err)
			return fr
		}
		slog.Info("upload: re-queued the failed upload for this copy", "path", fr.LocalPath, "node", failed.NodeUUID)
		return markUploadQueued(fr, failed.NodeUUID)
	}

	fullHash, fastHash := fr.Write.FullHash, fr.Write.FastHash
	if fullHash == "" {
		// The copy was already in place, so nothing was staged and read
		// this call: hash the local edit copy (cache-defeating, like the
		// already-present path of dual-write ingest).
		vr, verr := Verify(fr.LocalPath, "", e.progressOpts(ctx, fr.LocalPath, ProgressPhaseVerifying, info.Size())...)
		if verr != nil {
			slog.Warn("upload: could not hash the local copy to queue its upload; leaving the file deferred", "path", fr.LocalPath, "err", verr)
			return fr
		}
		fullHash = vr.Hash
		fastHash, err = fastHashFile(fr.LocalPath, info.Size())
		if err != nil {
			slog.Warn("upload: could not fast-hash the local copy to queue its upload; leaving the file deferred", "path", fr.LocalPath, "err", err)
			return fr
		}
	}

	nodeUUID, err := e.NewNodeUUID()
	if err != nil {
		slog.Warn("upload: could not mint a queue key for the deferred upload; leaving the file deferred", "path", fr.LocalPath, "err", err)
		return fr
	}
	kind := queue.KindMedia
	if it.isSidecar {
		kind = queue.KindSidecar
	}
	rec := queue.NewRecord{
		NodeUUID:         nodeUUID,
		Kind:             kind,
		Origin:           queue.OriginUpload,
		SourcePath:       it.srcPath,
		LocalPath:        fr.LocalPath,
		FileName:         filepath.Base(fr.LocalPath),
		FileExt:          extNoDot(fr.LocalPath),
		SizeBytes:        info.Size(),
		MtimeUnix:        info.ModTime().Unix(),
		FullHash:         fullHash,
		FastHash:         fastHash,
		UploadName:       it.uploadName,
		CameraModel:      it.cameraModel,
		CaptureTimestamp: it.captureTimestamp,
		SourcePathHash:   sourcePathHash(it.srcPath),
		MasterNodeUUID:   it.masterQueueID,
		SidecarTail:      it.sidecarTail,
		SidecarByName:    it.sidecarByName,
		LocalEditRoot:    e.Ingest.LocalEditRoot,
	}
	if err := e.Queue.InsertPending(durable, rec); err != nil {
		slog.Warn("upload: could not queue the deferred upload; leaving the file deferred", "path", fr.LocalPath, "err", err)
		return fr
	}
	slog.Info("upload: deferred upload queued for the drain", "path", fr.LocalPath)
	return markUploadQueued(fr, nodeUUID)
}

// markUploadQueued turns a Deferred copy result into the Queued one.
func markUploadQueued(fr FileResult, queueID string) FileResult {
	fr.Deferred = false
	fr.Skipped = false
	fr.SkipReason = ""
	fr.Queued = true
	fr.UploadQueueID = queueID
	return fr
}

// reconcileQueuedUpload closes the queue row of a deferral that a card
// re-insert just made redundant by uploading the file itself: without it
// Drain would upload the same bytes again. The row is found by the card file
// and its hash (the earlier deferred copy may or may not have been moved).
// movedFrom, when set, is the path an earlier deferred copy was moved from
// to finalLocal: the row that claims that path is the one to close (a card
// that came back under another mount point, or different card with the same
// bytes, would not match by source) and records finalLocal; otherwise the
// row keeps the deferred copy's own path.
// Best-effort: the upload already landed, so a failure here only costs Drain
// a redundant (server-deduplicated) upload.
func (e *Engine) reconcileQueuedUpload(ctx context.Context, it uploadItem, w WriteResult, resp *branchdam.UploadResponse, finalLocal, movedFrom string) {
	if e.Queue == nil {
		return
	}
	durable := context.WithoutCancel(ctx)
	var (
		rec   queue.Record
		found bool
		err   error
	)
	if movedFrom != "" {
		rec, found, err = e.Queue.UnfinishedUploadByLocalPath(durable, movedFrom)
	}
	if err == nil && !found {
		rec, found, err = e.Queue.UnfinishedUploadBySource(durable, it.srcPath, w.FullHash)
	}
	if err != nil {
		slog.Warn("upload: could not look up the queued upload this re-insert finished", "path", it.srcPath, "err", err)
		return
	}
	if !found {
		return
	}
	localPath := rec.LocalPath
	if movedFrom != "" && rec.LocalPath == movedFrom {
		localPath = finalLocal
	}
	if _, err := e.Queue.MarkUploadDone(durable, rec.NodeUUID, resp.NodeUUID, resp.RelativePath, localPath); err != nil {
		slog.Warn("upload: could not close the queued upload this re-insert finished", "path", it.srcPath, "node", rec.NodeUUID, "err", err)
	}
}

// uploadDrainer is what Drain's upload phase needs from the client: the
// same Upload call the engine's upload mode makes. Drain discovers it with a
// type assertion (like NewEngine) so a client without it -- a test fake --
// simply has no upload phase.
type uploadDrainer = uploader

// drainUploads is Drain's upload phase: it streams the verified local copy of
// every OriginUpload row still PENDING to the server, the way an upload-mode
// ingest would have if the link had allowed it, then moves the copy to the
// server's relativePath as a card re-insert does.
//
// It runs for both MEDIA and SIDECAR rows. A sidecar linked to a master
// (MasterNodeUUID) waits for the master's upload and is named after the
// master's server-assigned name (the same rule as uploadPass.masterFor); if
// the master FAILED it fails too. Rows are visited oldest first, so a master
// normally goes before its sidecars within one pass, but the link -- not the
// order -- is what holds a sidecar back while its master is backing off.
//
// A row is only uploaded if the local copy still has the size and mtime
// recorded when it was queued (the same baseline prune re-checks): an edited
// or missing copy fails the row rather than uploading bytes the recorded
// BLAKE3 no longer describes.
//
// uploadCtx bounds the streaming itself and is deliberately not the pass's
// context: the tray's pass deadline is minutes, a video upload can be longer.
// The marks after an upload use a context that outlives cancellation, so a
// finished upload is always recorded.
func drainUploads(ctx context.Context, up uploadDrainer, store *queue.Store, rows []queue.Record, nowT time.Time, o drainOptions, stats *DrainStats) {
	uctx := o.uploadCtx
	if uctx == nil {
		uctx = ctx
	}
	for _, r := range rows {
		if r.UploadStatus != queue.StatusPending || r.UploadNextAttemptUnix > nowT.Unix() {
			continue
		}
		if uctx.Err() != nil {
			return
		}
		if o.uploadGate != nil {
			// Fail closed, per row: the link can turn metered mid-pass.
			if metered, err := o.uploadGate(); metered || err != nil {
				slog.Info("drain: uploads held back on a metered connection", "err", err)
				return
			}
		}
		drainUploadOne(uctx, up, store, r, nowT, o, stats)
	}
}

func drainUploadOne(uctx context.Context, up uploadDrainer, store *queue.Store, r queue.Record, nowT time.Time, o drainOptions, stats *DrainStats) {
	durable := context.WithoutCancel(uctx)
	fail := func(msg string) {
		if err := store.MarkUploadFailed(durable, r.NodeUUID, msg); err == nil {
			stats.UploadsFailed++
		}
	}

	name := r.UploadName
	if r.MasterNodeUUID != "" {
		master, found, err := store.ByNodeUUID(durable, r.MasterNodeUUID)
		switch {
		case err != nil:
			return // transient: try again next pass, no attempt counted
		case !found:
			// The master's row was collected (only possible long after it
			// finished): the sidecar keeps the name it was queued with.
			slog.Warn("drain: a queued sidecar's master row is gone; uploading it under its queued name", "node", r.NodeUUID, "master", r.MasterNodeUUID)
		case master.UploadStatus == queue.StatusPending:
			return // the master is still to upload (or backing off)
		case master.UploadStatus == queue.StatusFailed:
			fail("master upload failed: " + master.UploadLastError)
			return
		case master.UploadStatus == queue.StatusDone:
			name = sidecarNameFromMaster(master.ServerRelativePath, r.SidecarTail, r.SidecarByName, name)
		}
	}

	// The baseline is the size and mtime recorded at enqueue, in whole
	// seconds: a cheap tripwire for an edited or replaced copy, not the
	// integrity guarantee. A same-size rewrite inside one second slips past
	// it and is caught by the server's check of the BLAKE3 the upload sends.
	info, err := os.Stat(r.LocalPath)
	if err != nil {
		fail(fmt.Sprintf("local copy unavailable: %v", err))
		return
	}
	if !info.Mode().IsRegular() || info.Size() != r.SizeBytes || info.ModTime().Unix() != r.MtimeUnix {
		fail("local copy changed since it was queued")
		return
	}

	f, err := os.Open(r.LocalPath) //nolint:gosec // r.LocalPath is the agent's own local edit copy
	if err != nil {
		_ = store.MarkUploadAttempt(durable, r.NodeUUID, fmt.Sprintf("open local copy: %v", err), backoffFor(r.UploadAttempts+1, nowT))
		return
	}
	defer func() { _ = f.Close() }()

	body := &progressReader{r: f}
	if o.progress != nil {
		body.onBytes = func(n int64) {
			o.progress(ProgressEvent{Path: r.LocalPath, Phase: ProgressPhaseUploading, BytesDone: n, TotalBytes: r.SizeBytes})
		}
	}
	resp, err := up.Upload(uctx, body, branchdam.UploadOptions{
		Filename:         name,
		CameraModel:      r.CameraModel,
		CaptureTimestamp: r.CaptureTimestamp,
		Blake3Hash:       r.FullHash,
		SourcePathHash:   r.SourcePathHash,
		SizeBytes:        r.SizeBytes,
	})
	// Closed before anything below can rename the file: Windows refuses to
	// rename a file that is still open. (The deferred Close above stays as
	// the safety net for the early returns; a second Close is harmless.)
	_ = f.Close()
	if err != nil {
		if uctx.Err() != nil {
			return // shutting down: not a failed attempt
		}
		var httpErr *branchdam.HTTPError
		if errors.Is(err, branchdam.ErrUploadHashMismatch) || (errors.As(err, &httpErr) && httpErr.Classification() == branchdam.ClassificationFatal) {
			fail(err.Error())
			return
		}
		_ = store.MarkUploadAttempt(durable, r.NodeUUID, err.Error(), backoffFor(r.UploadAttempts+1, nowT))
		return
	}
	if resp.Blake3Hash != "" && !strings.EqualFold(resp.Blake3Hash, r.FullHash) {
		fail(fmt.Sprintf("server stored BLAKE3 %s for node %s at %s, local copy is %s -- the server copy differs and should be removed",
			resp.Blake3Hash, resp.NodeUUID, resp.RelativePath, r.FullHash))
		return
	}

	// A card re-insert may have uploaded this file and closed the row while
	// the stream above ran (Drain holds no ingest gate: a long upload must
	// not block card ingests). Then the copy was already dealt with -- it
	// may have been moved -- so leave it alone.
	cur, found, err := store.ByNodeUUID(durable, r.NodeUUID)
	if err == nil && (!found || cur.UploadStatus != queue.StatusPending) {
		slog.Info("drain: a queued upload was finished by a card re-insert while it streamed", "node", r.NodeUUID)
		return
	}

	// Record first, move second: the row then never points at a path the
	// file has not reached nor leaves a finished upload PENDING with a path
	// the move already vacated (a crash between the two would read as a
	// "missing local copy" failure on the next pass). The compare-and-set
	// also settles a race with a re-insert before this side touches the file.
	target := plannedUploadMove(r, resp.RelativePath)
	localPath := r.LocalPath
	if target != "" {
		localPath = target
	}
	applied, err := store.MarkUploadDone(durable, r.NodeUUID, resp.NodeUUID, resp.RelativePath, localPath)
	switch {
	case err != nil:
		slog.Warn("drain: uploaded, but could not record it; the next pass re-uploads (the server deduplicates)", "node", r.NodeUUID, "err", err)
		return
	case !applied:
		slog.Warn("drain: a queued upload was closed by someone else while it streamed; leaving its copy alone", "node", r.NodeUUID)
		return
	}
	stats.UploadsDone++
	if target != "" {
		if err := moveNoClobber(r.LocalPath, target); err != nil {
			slog.Warn("drain: uploaded, but could not move the local copy", "local", r.LocalPath, "target", target, "err", err)
			// The copy is still where it was: say so, unless something else
			// already moved it (a re-insert), in which case the row stays.
			if _, statErr := os.Lstat(r.LocalPath); statErr == nil {
				if setErr := store.SetUploadLocalPath(durable, r.NodeUUID, r.LocalPath); setErr != nil {
					slog.Warn("drain: could not correct the row's local path", "node", r.NodeUUID, "err", setErr)
				}
			}
		}
	}
}

// plannedUploadMove is where a drained upload's local copy should go:
// LocalEditRoot/<server relativePath>, one copy in the edit tree where the
// server says the node lives, like moveDeferredCopy does for a re-insert. It
// returns "" when the copy should stay where it is (no root recorded, an
// unsafe path, the same path, or something already there -- each logged where
// it is unexpected; the upload counts regardless and nothing is overwritten).
func plannedUploadMove(r queue.Record, relativePath string) string {
	if r.LocalEditRoot == "" {
		return ""
	}
	rel, err := safeRelativePath(relativePath)
	if err != nil {
		slog.Warn("drain: uploaded, but the server's relativePath is unsafe; the local copy stays where it is", "local", r.LocalPath, "err", err)
		return ""
	}
	target := filepath.Join(r.LocalEditRoot, rel)
	if target == r.LocalPath {
		return ""
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("drain: uploaded; something is already at the server's path, so the local copy stays where it is", "local", r.LocalPath, "target", target)
		return ""
	}
	return target
}

// moveNoClobber moves src to dst, creating dst's directory, never overwriting.
func moveNoClobber(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := renameNoClobber(src, dst); err != nil {
		return err
	}
	if dir := filepath.Dir(src); dir != filepath.Dir(dst) {
		if dirErr := syncParentDirFn(dir); dirErr != nil {
			slog.Warn("drain: dir fsync failed; entry may not be durable on power loss", "dir", dir, "err", dirErr)
		}
	}
	slog.Info("drain: moved the uploaded local copy to the server's path", "from", src, "to", dst)
	return nil
}

// sidecarNameFromMaster is uploadPass.masterFor's naming rule applied to the
// master's server-assigned relativePath: the master's stem (or, for an
// "IMG_1.CR3.xmp" sidecar matched by full name, its whole base name) plus the
// sidecar's case-preserved tail. An empty relativePath keeps fallback.
func sidecarNameFromMaster(masterRelPath, tail string, byName bool, fallback string) string {
	if masterRelPath == "" {
		return fallback
	}
	base := path.Base(filepath.ToSlash(masterRelPath))
	if byName {
		return base + tail
	}
	stem, _ := splitBase(base)
	return stem + tail
}
