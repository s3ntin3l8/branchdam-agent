package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/naming"
	"github.com/s3ntin3l8/branchdam-agent/internal/phash"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// OfflineFileResult is one card file's outcome under IngestCardOffline.
// Unlike FileResult (IngestCard's online counterpart), there is no EventID
// here -- EVENT_NODE_CREATED submission (and the later archive copy and
// rebase) may not have happened yet by the time IngestCardOffline returns;
// see the Queued field and internal/ingest/drain.go's Drain.
type OfflineFileResult struct {
	SourcePath       string
	LocalPath        string
	IsSidecar        bool
	Skipped          bool
	SkipReason       string
	ExistingNodeUUID string
	NodeUUID         string

	// Queued is true once this file has a durable queue.db row -- the
	// crash-safety boundary. A restart after Queued=true is guaranteed to
	// resume this file's remaining steps without re-copying the local file
	// or re-minting NodeUUID (see ingestFileOffline's doc comment).
	Queued bool
	// AlreadyQueued is true when this run found an existing queue.db row for
	// SourcePath whose recorded size and mtime still match the file on the
	// card (a resume of a previous, interrupted run over the same card)
	// rather than writing a fresh local copy. A row for the same path whose
	// size or mtime differs is NOT a resume -- it describes a different file
	// that happened to have the same path (a reformatted card whose camera
	// restarted its file counter), and that file is copied as new work.
	AlreadyQueued bool
	// SubmittedInline is true if EVENT_NODE_CREATED was accepted (202)
	// during this call itself -- an opportunistic fast path for when the
	// server happens to be reachable even though the archive destination
	// isn't. Its absence does not mean failure: Drain retries it later
	// regardless.
	SubmittedInline bool
	LocalVerify     VerifyResult
	PHash           *int64
	Err             error
}

// OfflineCardResult is one IngestCardOffline call's full outcome.
type OfflineCardResult struct {
	Files []OfflineFileResult
}

// OfflineOutcome is the single bucket an OfflineFileResult is reported
// under by the CLI's `ingest -offline` report and the tray's IngestSummary
// -- one classification shared by both, so the two can't drift on what
// counts as safe-to-eject.
type OfflineOutcome int

const (
	// OfflineOutcomeFailed: the file was neither written and queued in this
	// run nor genuinely resumed from an earlier one, and wasn't deliberately
	// skipped either. Any file in this bucket means the card is NOT safe to
	// eject.
	OfflineOutcomeFailed OfflineOutcome = iota
	// OfflineOutcomeQueued: a fresh local copy was written, verified and
	// durably queued in this run.
	OfflineOutcomeQueued
	// OfflineOutcomeResumed: an earlier run already copied and queued this
	// exact file (same path, size and mtime); nothing was re-copied.
	OfflineOutcomeResumed
	// OfflineOutcomeSkipped: a deliberate skip (OS metadata, extension
	// filter, identical copy already at the destination, server-side
	// duplicate, or a freshly queued sidecar).
	OfflineOutcomeSkipped
)

// Outcome classifies f. Err always wins; a resume is reported separately
// from new work; and a result that is none of failed/resumed/skipped/queued
// is treated as a failure rather than silently counted as success.
func (f OfflineFileResult) Outcome() OfflineOutcome {
	switch {
	case f.Err != nil:
		return OfflineOutcomeFailed
	case f.AlreadyQueued && f.Queued:
		return OfflineOutcomeResumed
	case f.Skipped:
		return OfflineOutcomeSkipped
	case f.Queued:
		return OfflineOutcomeQueued
	default:
		return OfflineOutcomeFailed
	}
}

// IngestCardOffline walks cardRoot exactly like IngestCard, but never
// attempts to write the archive destination or submit EVENT_NODE_CREATED
// synchronously as a required step: it writes the local edit copy only,
// mints and persists a queue.db row per file, and makes one opportunistic
// attempt at EVENT_NODE_CREATED (harmless if it fails -- Drain, run later on
// reconnect, is the source of truth for everything this call queues). See
// the plan's M2 milestone and issue #4's "Offline ingest flow" for the
// design this implements: local copy immediate/unconditional, archive copy
// and rebase deferred to Drain.
//
// e.Queue must be set (non-nil) -- this is the offline entry point and has
// no fallback if there's nowhere durable to record intent.
//
// Issue #100: the same by-name and by-extension pre-filters
// IngestCard applies run here, for the same reasons (a macOS-formatted
// card's .DS_Store in every directory must not become a queue.db row).
// Skipped files still appear in OfflineCardResult.Files, marked
// OfflineFileResult{Skipped: true, SkipReason: "OS metadata: ..."}, so
// `ingest -offline --card <path>` is symmetric with the online path.
//
// Partial-application semantics mirror IngestCard's exactly:
// ingestFileOffline runs inline inside the WalkDir callback, so a
// mid-walk error leaves already-queued rows durable and a re-run
// is always safe (BySourcePath at the top of each per-file path
// short-circuits the re-run when the queued row's size and mtime still
// match the source file, no work redone).
func (e *Engine) IngestCardOffline(ctx context.Context, cardRoot string) (OfflineCardResult, error) {
	if e.Queue == nil {
		return OfflineCardResult{}, fmt.Errorf("ingest: IngestCardOffline requires a non-nil Engine.Queue")
	}

	var dedupUnavailable bool
	if checker, ok := e.Client.(contentChecker); ok && e.Ingest.PreflightTimeoutSecs >= 0 {
		probeTimeout := time.Duration(config.DefaultOfflinePreflightTimeoutSecs) * time.Second
		if e.Ingest.PreflightTimeoutSecs > 0 {
			probeTimeout = time.Duration(e.Ingest.PreflightTimeoutSecs) * time.Second
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, probeTimeout)
		_, probeErr := checker.CheckContent(probeCtx, "", "")
		probeCancel()
		if probeErr != nil {
			var he *branchdam.HTTPError
			if !errors.As(probeErr, &he) || (he.StatusCode == http.StatusNotFound || he.StatusCode == http.StatusNotImplemented || he.StatusCode == http.StatusBadRequest) {
				dedupUnavailable = true
				slog.Warn("ingest: offline pre-flight dedup endpoint unavailable -- skipping pre-flight checks for this run", "err", probeErr)
			} else {
				slog.Warn("ingest: offline pre-flight probe returned error (proceeding per-file)", "err", probeErr)
			}
		}
	}

	// Probed once per card, not per file: whether ResolveDestination may
	// also look at the archive for name collisions (see
	// resolveOfflineDestination). Offline ingest usually runs precisely
	// because the archive is unreachable; when it isn't (an explicit
	// `ingest -offline` with the NAS mounted), checking it up front means a
	// new file never gets a name an existing master already holds.
	archiveReachable := e.archiveReachable(ctx)

	stemSuffix := make(map[string]string)
	var result OfflineCardResult
	err := filepath.WalkDir(cardRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if skip, reason := shouldSkipByName(path); skip {
			result.Files = append(result.Files, OfflineFileResult{
				SourcePath: path,
				Skipped:    true,
				SkipReason: reason,
			})
			return nil
		}
		if shouldSkipByExtension(e.Ingest.AllowedExtensions, extNoDot(path)) {
			result.Files = append(result.Files, OfflineFileResult{
				SourcePath: path,
				Skipped:    true,
				SkipReason: fmt.Sprintf("extension %q not in allowedExtensions", extNoDot(path)),
			})
			return nil
		}
		result.Files = append(result.Files, e.ingestFileOffline(ctx, path, stemSuffix, &dedupUnavailable, archiveReachable))
		return nil
	})
	if err != nil {
		return OfflineCardResult{}, fmt.Errorf("ingest: walk card root %s: %w", cardRoot, err)
	}
	return result, nil
}

// ingestFileOffline is IngestCardOffline's per-file worker. Ordering is the
// entire point of this function, so it is spelled out here rather than left
// implicit:
//
//  1. Check queue.db for an existing row keyed by this source path whose
//     recorded size and mtime match the source file as it is now -- a
//     restart resuming a previously-interrupted run over the same card. If
//     found, trust it (the row only exists because step 5 below already
//     completed on the earlier run) and return without touching the
//     filesystem again. Queue rows are never deleted and cameras restart
//     their file counters after a format, so a path match alone is not
//     evidence of a resume: a row whose size or mtime differs belongs to a
//     different file that used to live at this path, and the current file
//     is ingested as new work (it gets its own row, NodeUUID and, via
//     ResolveDestination, its own suffixed destination). Size+mtime come
//     from the stat this function already does; no extra read of the card
//     is needed (invariant 1: the card is read once, by WriteLocal).
//     A resumed row whose local copy never made it to its final name (a
//     crash between step 6 and step 7 below) is completed here: the
//     agent's own temp copy is verified and renamed into place, or
//     re-copied from the card if it's missing or bad.
//  2. Pick the destination name (resolveOfflineDestination): if an
//     identical file is already at the destination AND a queue row claims
//     it (or the reachable archive holds the same file), skip. A name
//     occupied by anything else -- an online-ingest edit copy, a file the
//     user put there, another card's master in the reachable archive -- is
//     never reused and never deleted; the new file gets the next suffix.
//  3. Pre-flight content dedup check: if server is reachable and pre-flight
//     is enabled, query GET /api/v1/agent/check-content with fastHash and
//     fullHash. If confirmed duplicate with active lifecycle state, skip
//     without writing local copy or deleting existing files.
//  4. Remove the agent's own leftover temp copy for this destination, if
//     any (its name, localTempName, marks it as the agent's: nothing else
//     is ever deleted).
//  5. Write and verify the local copy under that temp name. This step has
//     no durable side effect yet -- if the process dies here, restart lands
//     back at step 4 with nothing to resume, which is correct: nothing was
//     ever promised to branchDAM about this file, and no file sits at a
//     final name for a later run to mistake for an ingested one.
//  6. Mint NodeUUID, build both container paths and the NodeCreatedPayload,
//     and INSERT the queue.db row. This commit is the durability boundary:
//     once it returns, the file's identity and intent are permanent, and
//     every step after this point is safe to retry indefinitely because it
//     is idempotent against that same NodeUUID.
//  7. Rename the temp copy to its final local name, refusing to replace
//     anything already there. Only now does a file appear at a final name,
//     and it is already claimed by the row from step 6.
//  8. Only after the row is durably queued, make one best-effort attempt at
//     POST EVENT_NODE_CREATED. Whether this succeeds or fails is not
//     load-bearing for correctness -- Drain retries it either way -- it
//     just means a workstation that's actually online (just missing the NAS
//     mount, say) doesn't wait for the next Drain pass to start tracking the
//     node server-side.
//
// Sidecars (.xmp/.srt) skip step 8 entirely: no NodeCreatedPayload, no
// EVENT_NODE_CREATED, no rebase -- only the local write and the archive-copy
// queue row, mirroring IngestCard's sidecar handling.
func (e *Engine) ingestFileOffline(ctx context.Context, srcPath string, stemSuffix map[string]string, dedupUnavailable *bool, archiveReachable bool) OfflineFileResult {
	fr := OfflineFileResult{SourcePath: srcPath}

	ext := extNoDot(srcPath)
	fr.IsSidecar = ext == "xmp" || ext == "srt"

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		fr.Err = fmt.Errorf("stat source: %w", err)
		return fr
	}

	existing, found, err := e.Queue.BySourcePath(ctx, srcPath)
	if err != nil {
		// Fail closed: without the lookup we can't tell a resume from new
		// work, and guessing either way is wrong (a false resume loses the
		// file; a false "new" duplicates it). Failing the file keeps the
		// card from being reported safe to eject.
		fr.Err = fmt.Errorf("queue lookup by source path: %w", err)
		return fr
	}
	if found {
		if sameSourceFile(existing, srcInfo) {
			fr.AlreadyQueued = true
			fr.Queued = true
			fr.NodeUUID = existing.NodeUUID
			fr.LocalPath = existing.LocalPath
			fr.Skipped = fr.IsSidecar
			if fr.IsSidecar {
				fr.SkipReason = "sidecar file (.xmp/.srt): copied to both destinations eventually, no EVENT_NODE_CREATED submitted"
			}
			if err := e.completeResumedLocalCopy(srcPath, srcInfo, existing); err != nil {
				fr.Err = fmt.Errorf("resume: restore local copy %s: %w", existing.LocalPath, err)
			}
			return fr
		}
		slog.Info("ingest: offline queue has a row for this source path with a different size/mtime -- ingesting as a new file",
			"source", srcPath,
			"queuedSize", existing.SizeBytes, "queuedMtime", existing.MtimeUnix,
			"size", srcInfo.Size(), "mtime", srcInfo.ModTime().Unix())
	}

	var exif *ExifResult
	if e.Exiftool != nil && e.Exiftool.HasExiftool() && !fr.IsSidecar {
		if res, err := e.Exiftool.Exif(ctx, srcPath); err == nil {
			exif = res
		}
	}

	vars := TemplateVars{OriginalName: filepath.Base(srcPath)}
	if exif != nil && exif.CapturedAt != nil {
		vars.CapturedAt = *exif.CapturedAt
	} else {
		vars.CapturedAt = srcInfo.ModTime()
	}
	if exif != nil {
		vars.CameraModel = exif.CameraModel
	}

	stem, _ := splitBase(filepath.Base(srcPath))
	stemKey := filepath.Join(filepath.Dir(srcPath), stem)
	knownSuffix := ""
	if stemSuffix != nil {
		knownSuffix = stemSuffix[stemKey]
	}

	resolution, err := e.resolveOfflineDestination(ctx, vars, srcPath, knownSuffix, archiveReachable)
	if err != nil {
		fr.Err = fmt.Errorf("resolve destination: %w", err)
		return fr
	}
	if stemSuffix != nil && resolution.Suffix != "" {
		stemSuffix[stemKey] = resolution.Suffix
	}

	relPath := resolution.RelPath
	archivePath := filepath.Join(e.Ingest.ArchiveRoot, relPath)
	localPath := filepath.Join(e.Ingest.LocalEditRoot, relPath)
	fr.LocalPath = localPath

	if resolution.AlreadyIngested {
		fr.Skipped = true
		fr.SkipReason = "already ingested (identical file exists at destination)"
		return fr
	}

	// Pre-flight BLAKE3 content dedup check before WriteLocal.
	if !fr.IsSidecar {
		if skip, existingUUID, skipReason := e.checkContentDedup(ctx, srcPath, srcInfo.Size(), config.DefaultOfflinePreflightTimeoutSecs, dedupUnavailable); skip {
			fr.Skipped = true
			fr.SkipReason = skipReason
			fr.ExistingNodeUUID = existingUUID
			return fr
		}
	}

	// The local copy is written under the agent's own temp name and only
	// renamed to localPath after the queue row claiming it is committed
	// (see placeLocal below). A leftover at this temp name is the agent's
	// own, from a run that died before its row was inserted -- removing it
	// is the only cleanup this path ever does. Until the row is in,
	// every early return removes the temp again (rowInserted guards that).
	tmpLocal := localTempName(localPath)
	_ = os.Remove(tmpLocal)
	rowInserted := false
	defer func() {
		if !rowInserted {
			_ = os.Remove(tmpLocal)
		}
	}()
	// Once rowInserted is true the temp is deliberately kept on a failed
	// rename: the row claims it, and a re-run completes the move.
	placeLocal := func() bool {
		if err := renameNoClobber(tmpLocal, localPath); err != nil {
			// The row is in; keep the temp copy. A re-run over this card
			// resumes the row and retries the rename
			// (completeResumedLocalCopy).
			fr.Err = fmt.Errorf("move local copy into place: %w", err)
			return false
		}
		return true
	}

	writeRes, err := WriteLocal(srcPath, tmpLocal, e.progressOpts(localPath, ProgressPhaseCopying, srcInfo.Size())...)
	if err != nil {
		fr.Err = fmt.Errorf("write local copy: %w", err)
		return fr
	}
	// Soft contract (issue #103): the local-path mtime is what
	// prune.go's TOCTOU re-stat reads back, so a silently-swallowed
	// Chtimes failure here means the prune-safety half of invariant
	// #8 quietly stops catching anything. Log it; don't fail ingest.
	// (The rename into place below keeps the mtime.)
	preserveMtimeAt(srcPath, tmpLocal, srcInfo.ModTime())

	localVerify, err := Verify(tmpLocal, writeRes.FullHash, e.progressOpts(localPath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
	if err != nil {
		fr.Err = fmt.Errorf("verify local copy: %w", err)
		return fr
	}
	localVerify.Path = localPath
	fr.LocalVerify = localVerify
	if !localVerify.Verified {
		fr.Err = fmt.Errorf("ingest: local copy verification failed for %s -- safe-eject withheld", srcPath)
		return fr
	}

	if e.Ingest.RequireUnbuffered && localVerify.Method == VerifyMethodBufferedFloor {
		fr.Err = fmt.Errorf("ingest: unbuffered verify required by config, but local verify fell back to buffered floor (%s) -- safe-eject withheld", localVerify.Method)
		return fr
	}

	if fr.IsSidecar {
		fr.Skipped = true
		fr.SkipReason = "sidecar file (.xmp/.srt): copied to both destinations eventually, no EVENT_NODE_CREATED submitted"
		archiveContainerPath, cpErr := ToContainerPath(e.Mappings, archivePath)
		if cpErr != nil {
			fr.Err = cpErr
			return fr
		}
		sidecarUUID, err := e.NewNodeUUID()
		if err != nil {
			fr.Err = fmt.Errorf("mint node uuid: %w", err)
			return fr
		}
		rec := queue.NewRecord{
			NodeUUID:             sidecarUUID,
			Kind:                 queue.KindSidecar,
			SourcePath:           srcPath,
			LocalPath:            localPath,
			ArchivePath:          archivePath,
			ArchiveContainerPath: archiveContainerPath,
			FileName:             filepath.Base(archivePath),
			FileExt:              ext,
			SizeBytes:            writeRes.SizeBytes,
			MtimeUnix:            srcInfo.ModTime().Unix(),
			FullHash:             writeRes.FullHash,
			FastHash:             writeRes.FastHash,
		}
		if err := e.Queue.InsertPending(ctx, rec); err != nil {
			fr.Err = err
			return fr
		}
		rowInserted = true
		fr.NodeUUID = rec.NodeUUID
		fr.Queued = true
		placeLocal()
		return fr
	}

	if e.Exiftool != nil && isImageExt(ext) {
		if ph, err := phash.Extract(ctx, e.Exiftool.Pool(), tmpLocal); err == nil {
			fr.PHash = ph
		}
	}

	gpsLat, gpsLon := exifGPS(exif)
	if gpsLat == nil && isVideoExt(ext) {
		if srtPath, ok := findSRTSidecar(srcPath); ok {
			if lat, lon, ok, err := srtGPS(srtPath); err == nil && ok {
				gpsLat, gpsLon = &lat, &lon
			}
		}
	}

	nodeUUID, err := e.NewNodeUUID()
	if err != nil {
		fr.Err = fmt.Errorf("mint node uuid: %w", err)
		return fr
	}
	fr.NodeUUID = nodeUUID

	archiveContainerPath, err := ToContainerPath(e.Mappings, archivePath)
	if err != nil {
		fr.Err = err
		return fr
	}
	tier0Path := e.Tier0ContainerRoot
	if tier0Path == "" {
		fr.Err = fmt.Errorf("ingest: Engine.Tier0ContainerRoot is empty -- required for offline ingest")
		return fr
	}
	tier0ContainerPath := joinContainerPath(tier0Path, relPath)

	payload := branchdam.NodeCreatedPayload{
		NodeUUID:     nodeUUID,
		FilePath:     tier0ContainerPath,
		FileName:     filepath.Base(archivePath),
		FileExt:      ext,
		SizeBytes:    writeRes.SizeBytes,
		MtimeUnix:    srcInfo.ModTime().Unix(),
		FastHash:     &writeRes.FastHash,
		FullHash:     &writeRes.FullHash,
		Phash:        fr.PHash,
		FilenameStem: strPtr(naming.Stem(filepath.Base(archivePath))),
		GPSLatitude:  gpsLat,
		GPSLongitude: gpsLon,
	}
	if exif != nil {
		payload.CameraModel = strPtr(exif.CameraModel)
		payload.CameraSerial = strPtr(exif.CameraSerial)
		payload.LensModel = strPtr(exif.LensModel)
		payload.OriginalDocumentID = strPtr(exif.OriginalDocumentID)
		payload.DocumentID = strPtr(exif.DocumentID)
		payload.DerivedFromID = strPtr(exif.DerivedFromID)
		if exif.CapturedAt != nil {
			unix := exif.CapturedAt.Unix()
			payload.CapturedAtUnix = &unix
		}
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		fr.Err = fmt.Errorf("marshal node created payload: %w", err)
		return fr
	}

	eventUUID := e.mintEventUUID()

	rec := queue.NewRecord{
		NodeUUID:               nodeUUID,
		Kind:                   queue.KindMedia,
		SourcePath:             srcPath,
		LocalPath:              localPath,
		ArchivePath:            archivePath,
		ArchiveContainerPath:   archiveContainerPath,
		Tier0ContainerPath:     tier0ContainerPath,
		FileName:               filepath.Base(archivePath),
		FileExt:                ext,
		SizeBytes:              writeRes.SizeBytes,
		MtimeUnix:              srcInfo.ModTime().Unix(),
		FullHash:               writeRes.FullHash,
		FastHash:               writeRes.FastHash,
		NodeCreatedPayloadJSON: string(payloadJSON),
		NodeCreatedEventUUID:   eventUUID,
	}

	// This InsertPending call is the durability boundary described in this
	// function's doc comment -- no network call happens before it.
	if err := e.Queue.InsertPending(ctx, rec); err != nil {
		fr.Err = err
		return fr
	}
	rowInserted = true
	fr.Queued = true
	if !placeLocal() {
		return fr
	}

	// Opportunistic fast path, strictly best-effort: failure here changes
	// nothing about correctness, since Drain will retry regardless (see this
	// function's doc comment, step 8).
	resp, postErr := e.Client.PostNodeCreatedWithUUID(ctx, e.AgentID, payload, rec.NodeCreatedEventUUID)
	if postErr == nil {
		if markErr := e.Queue.MarkNodeCreatedSubmitted(ctx, nodeUUID, resp.EventID, e.now()); markErr == nil {
			fr.SubmittedInline = true
		}
	}

	return fr
}

// sameSourceFile reports whether rec (the newest queue.db row recorded for
// a source path) describes the file currently at that path, by the size and
// whole-second mtime ingestFileOffline stored from its own pre-copy stat.
// Whole seconds because that is what the row stores (MtimeUnix), and the
// value is compared against the same filesystem's own stat on both sides.
//
// Residual risk, accepted: a different file with the same size that lands at
// the same path within the same wall-clock second as the queued one is taken
// for a resume and not copied. Telling them apart would need a content hash
// of the source, i.e. a second read of the card, which Key Invariant 1
// forbids.
func sameSourceFile(rec queue.Record, info os.FileInfo) bool {
	return rec.SizeBytes == info.Size() && rec.MtimeUnix == info.ModTime().Unix()
}

// joinContainerPath joins a container-path root and a slash-separated
// relative path, matching ToContainerPath's own trailing-slash handling so
// the Tier-0 path built here and an archive container path built via
// ToContainerPath look identical in shape.
func joinContainerPath(root, relPath string) string {
	root = strings.TrimRight(root, "/")
	if relPath == "" {
		return root
	}
	return root + "/" + relPath
}

// localTempName is the agent's temp name for an offline local edit copy:
// agentTempName plus the original extension again, so exiftool-based
// pHash extraction (which runs before the copy is renamed into place) can
// still tell a RAW from a JPEG by name.
func localTempName(localPath string) string {
	return agentTempName(localPath) + filepath.Ext(localPath)
}

// archiveProbeTimeout bounds archiveReachable's stat: a dead SMB/NFS mount
// can block a stat for minutes, and the probe must never stall an ingest
// that is offline precisely because the NAS is gone. Matches the tray's
// own archive probe (cmd/branchdam-agent's probeArchive).
const archiveProbeTimeout = 2 * time.Second

// archiveReachable reports whether Ingest.ArchiveRoot is currently a
// usable directory, via e.ArchiveReachable when set (tests) or a stat
// bounded by archiveProbeTimeout. On a hung mount the stat goroutine
// outlives the timeout: it stays parked in the syscall until the kernel
// returns, then writes to the buffered channel and exits. That is at most
// one goroutine per offline ingest pass, the same trade-off as
// cmd/branchdam-agent's probeArchive.
func (e *Engine) archiveReachable(ctx context.Context) bool {
	if e.ArchiveReachable != nil {
		return e.ArchiveReachable(ctx, e.Ingest.ArchiveRoot)
	}
	root := e.Ingest.ArchiveRoot
	if root == "" {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, archiveProbeTimeout)
	defer cancel()
	ch := make(chan bool, 1)
	go func() {
		info, err := os.Stat(root)
		ch <- err == nil && info.IsDir()
	}()
	select {
	case ok := <-ch:
		return ok
	case <-probeCtx.Done():
		return false
	}
}

// resolveOfflineDestination picks the relative destination for an offline
// ingest. It starts from ResolveDestination over the local edit root (the
// root every offline copy is written to), then vets the answer with
// offlineSlotUsable; if the slot isn't usable it walks the suffix space for
// one that is. Nothing found at a candidate name is ever deleted -- an
// occupied name is simply not used.
func (e *Engine) resolveOfflineDestination(ctx context.Context, vars TemplateVars, srcPath, knownSuffix string, archiveReachable bool) (DestinationResolution, error) {
	tpl := e.Ingest.PathTemplate
	res := ResolveDestination([]string{e.Ingest.LocalEditRoot}, tpl, vars, srcPath, knownSuffix)
	ok, err := e.offlineSlotUsable(ctx, res, srcPath, archiveReachable)
	if err != nil {
		return DestinationResolution{}, err
	}
	if ok {
		return res, nil
	}

	for counter := 1; counter <= 10000; counter++ {
		suffix := ""
		if counter > 1 {
			suffix = fmt.Sprintf("_%d", counter)
		}
		cv := vars
		cv.OriginalName = SuffixedFilename(vars.OriginalName, suffix)
		cand := DestinationResolution{RelPath: RenderPath(tpl, cv), Suffix: suffix}
		// Anything already at the local name -- a regular file, a
		// directory, a symlink -- makes it unusable; only "nothing there"
		// is a candidate.
		if _, err := os.Lstat(filepath.Join(e.Ingest.LocalEditRoot, cand.RelPath)); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return DestinationResolution{}, fmt.Errorf("stat local destination candidate: %w", err)
		}
		ok, err := e.offlineSlotUsable(ctx, cand, srcPath, archiveReachable)
		if err != nil {
			return DestinationResolution{}, err
		}
		if ok {
			return cand, nil
		}
	}
	return DestinationResolution{}, fmt.Errorf("no free destination name for %s after 10000 suffixes", srcPath)
}

// offlineSlotUsable vets one ResolveDestination answer for the offline path:
//
//   - AlreadyIngested (an identical file already sits at the local name) is
//     trusted only when that local file is claimed by a queue row -- it is
//     then already on its way to the archive -- or when the reachable
//     archive holds the same file at the same name. An identical local file
//     nobody queued (a copy from a run that predates the agent's temp-name
//     discipline, or one the user made) proves nothing about the archive,
//     and "skipping" on its strength would let the card be ejected with the
//     file never archived.
//   - A free local name is usable unless a queue row already claims it (that
//     row's copy is not in place yet and must keep its name) or, with the
//     archive reachable, a different file occupies the same archive name.
//     An identical archive file is fine: Drain finds it verified and copies
//     nothing. An archive stat error other than "not there" is treated as
//     "unknown" -- CopyToArchive refuses a conflicting name at drain time.
func (e *Engine) offlineSlotUsable(ctx context.Context, res DestinationResolution, srcPath string, archiveReachable bool) (bool, error) {
	localPath := filepath.Join(e.Ingest.LocalEditRoot, res.RelPath)
	archivePath := filepath.Join(e.Ingest.ArchiveRoot, res.RelPath)

	_, claimed, err := e.Queue.ByLocalPath(ctx, localPath)
	if err != nil {
		return false, fmt.Errorf("queue lookup by local path: %w", err)
	}

	if res.AlreadyIngested {
		if claimed {
			return true, nil
		}
		return archiveReachable && filesMatch(srcPath, archivePath), nil
	}

	if claimed {
		return false, nil
	}
	if archiveReachable && e.Ingest.ArchiveRoot != "" {
		info, err := os.Lstat(archivePath)
		switch {
		case err == nil && !info.Mode().IsRegular():
			// A directory/symlink/device at the archive name: CopyToArchive
			// would refuse it, so don't hand the name out.
			return false, nil
		case err == nil:
			return filesMatch(srcPath, archivePath), nil
		case errors.Is(err, fs.ErrNotExist):
			return true, nil
		default:
			// Unknown (permission, ENOTDIR, I/O): usable for the local
			// write; CopyToArchive refuses a real conflict at drain time.
			return true, nil
		}
	}
	return true, nil
}

// completeResumedLocalCopy makes sure a resumed row still has its local
// copy at rec.LocalPath while the archive copy -- which reads it -- is
// pending. The row is inserted before the copy is renamed into place
// (ingestFileOffline's steps 6-7), so a run that died in between leaves the
// row claiming a name nothing sits at yet. Recovery, in order: the agent's
// own temp copy, if it verifies against the queued hash, is renamed into
// place; otherwise (missing or bad) the file is copied again from the card
// -- this re-run's single read of it -- and must hash to the queued value.
// A row whose archive copy is no longer pending is left alone: prune may
// legitimately have removed its local copy.
func (e *Engine) completeResumedLocalCopy(srcPath string, srcInfo os.FileInfo, rec queue.Record) error {
	if rec.ArchiveCopyStatus != queue.StatusPending {
		return nil
	}
	if _, err := os.Lstat(rec.LocalPath); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	tmp := localTempName(rec.LocalPath)
	if info, err := os.Stat(tmp); err == nil && info.Mode().IsRegular() && info.Size() == rec.SizeBytes {
		if vr, verr := Verify(tmp, rec.FullHash); verr == nil && vr.Verified {
			return renameNoClobber(tmp, rec.LocalPath)
		}
	}
	_ = os.Remove(tmp) // the agent's own temp name: missing, short or corrupt

	writeRes, err := WriteLocal(srcPath, tmp, e.progressOpts(rec.LocalPath, ProgressPhaseCopying, srcInfo.Size())...)
	if err != nil {
		return err
	}
	if writeRes.FullHash != rec.FullHash {
		_ = os.Remove(tmp)
		return fmt.Errorf("card file now hashes to %s, queued as %s", writeRes.FullHash, rec.FullHash)
	}
	preserveMtimeAt(srcPath, tmp, srcInfo.ModTime())
	vr, err := Verify(tmp, writeRes.FullHash, e.progressOpts(rec.LocalPath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
	if err != nil || !vr.Verified {
		_ = os.Remove(tmp)
		if err == nil {
			err = fmt.Errorf("re-copied local file did not verify")
		}
		return err
	}
	return renameNoClobber(tmp, rec.LocalPath)
}
