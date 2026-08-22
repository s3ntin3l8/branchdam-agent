package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
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
	SourcePath string
	LocalPath  string
	IsSidecar  bool
	Skipped    bool
	SkipReason string
	NodeUUID   string
	// Queued is true once this file has a durable queue.db row -- the
	// crash-safety boundary. A restart after Queued=true is guaranteed to
	// resume this file's remaining steps without re-copying the local file
	// or re-minting NodeUUID (see ingestFileOffline's doc comment).
	Queued bool
	// AlreadyQueued is true when this run found an existing queue.db row for
	// SourcePath (a resume of a previous, interrupted run over the same
	// card) rather than writing a fresh local copy.
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
func (e *Engine) IngestCardOffline(ctx context.Context, cardRoot string) (OfflineCardResult, error) {
	if e.Queue == nil {
		return OfflineCardResult{}, fmt.Errorf("ingest: IngestCardOffline requires a non-nil Engine.Queue")
	}

	var files []string
	err := filepath.WalkDir(cardRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return OfflineCardResult{}, fmt.Errorf("ingest: walk card root %s: %w", cardRoot, err)
	}

	stemSuffix := make(map[string]string)
	var result OfflineCardResult
	for _, f := range files {
		fr := e.ingestFileOffline(ctx, f, stemSuffix)
		result.Files = append(result.Files, fr)
	}
	return result, nil
}

// ingestFileOffline is IngestCardOffline's per-file worker. Ordering is the
// entire point of this function, so it is spelled out here rather than left
// implicit:
//
//  1. Check queue.db for an existing row keyed by this source path -- a
//     restart resuming a previously-interrupted run over the same card. If
//     found, trust it (the row only exists because step 4 below already
//     completed on the earlier run) and return without touching the
//     filesystem again.
//  2. Otherwise, before writing anything: if localPath already exists on
//     disk but no queue row claims it, it's an orphaned partial from a run
//     that crashed between step 3 and step 4 below -- remove it so
//     WriteLocal's O_EXCL doesn't wedge forever (mirrors DualWrite's own
//     "caller must clean up a surviving partial before retrying" contract).
//  3. Write and verify the local copy. This step has no server-durable side
//     effect yet -- if the process dies here, restart lands back at step 2
//     with nothing to resume, which is correct: nothing was ever promised
//     to branchDAM about this file.
//  4. Mint NodeUUID, build both container paths and the NodeCreatedPayload,
//     and INSERT the queue.db row. This commit is the durability boundary:
//     once it returns, the file's identity and intent are permanent, and
//     every step after this point is safe to retry indefinitely because it
//     is idempotent against that same NodeUUID.
//  5. Only after the row is durably queued, make one best-effort attempt at
//     POST EVENT_NODE_CREATED. Whether this succeeds or fails is not
//     load-bearing for correctness -- Drain retries it either way -- it
//     just means a workstation that's actually online (just missing the NAS
//     mount, say) doesn't wait for the next Drain pass to start tracking the
//     node server-side.
//
// Sidecars (.xmp/.srt) skip steps 4-5 entirely: no NodeCreatedPayload, no
// EVENT_NODE_CREATED, no rebase -- only the local write and the archive-copy
// queue row, mirroring IngestCard's sidecar handling.
func (e *Engine) ingestFileOffline(ctx context.Context, srcPath string, stemSuffix map[string]string) OfflineFileResult {
	fr := OfflineFileResult{SourcePath: srcPath}

	ext := extNoDot(srcPath)
	fr.IsSidecar = ext == "xmp" || ext == "srt"

	if existing, ok, err := e.Queue.BySourcePath(ctx, srcPath); err == nil && ok {
		fr.AlreadyQueued = true
		fr.Queued = true
		fr.NodeUUID = existing.NodeUUID
		fr.LocalPath = existing.LocalPath
		fr.Skipped = fr.IsSidecar
		if fr.IsSidecar {
			fr.SkipReason = "sidecar file (.xmp/.srt): copied to both destinations eventually, no EVENT_NODE_CREATED submitted"
		}
		return fr
	}

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		fr.Err = fmt.Errorf("stat source: %w", err)
		return fr
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

	primaryRel := RenderPath(e.Ingest.PathTemplate, vars)
	primaryLocal := filepath.Join(e.Ingest.LocalEditRoot, primaryRel)
	if _, err := os.Stat(primaryLocal); err == nil {
		if e.Queue != nil {
			if _, claimed, err := e.Queue.ByLocalPath(ctx, primaryLocal); err == nil && !claimed {
				_ = os.Remove(primaryLocal)
			}
		}
	}

	stem, _ := splitBase(filepath.Base(srcPath))
	stemKey := filepath.Join(filepath.Dir(srcPath), stem)
	knownSuffix := ""
	if stemSuffix != nil {
		knownSuffix = stemSuffix[stemKey]
	}

	roots := []string{e.Ingest.LocalEditRoot}
	resolution := ResolveDestination(roots, e.Ingest.PathTemplate, vars, srcPath, knownSuffix)
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

	// An orphaned partial from a run that crashed after WriteLocal but
	// before the queue row was inserted: nothing durable promised this file
	// exists yet, so it's safe -- and necessary, or O_EXCL below wedges
	// forever -- to remove it and start over.
	if _, err := os.Stat(localPath); err == nil {
		_ = os.Remove(localPath)
	}

	writeRes, err := WriteLocal(srcPath, localPath)
	if err != nil {
		fr.Err = fmt.Errorf("write local copy: %w", err)
		return fr
	}
	_ = os.Chtimes(localPath, e.now(), srcInfo.ModTime())

	localVerify, err := Verify(localPath, writeRes.FullHash)
	if err != nil {
		_ = os.Remove(localPath)
		fr.Err = fmt.Errorf("verify local copy: %w", err)
		return fr
	}
	fr.LocalVerify = localVerify
	if !localVerify.Verified {
		_ = os.Remove(localPath)
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
		fr.NodeUUID = rec.NodeUUID
		fr.Queued = true
		return fr
	}

	if e.Exiftool != nil && isImageExt(ext) {
		if ph, err := phash.Extract(ctx, e.Exiftool.Path(), localPath); err == nil {
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
	}

	// This InsertPending call is the durability boundary described in this
	// function's doc comment -- no network call happens before it.
	if err := e.Queue.InsertPending(ctx, rec); err != nil {
		fr.Err = err
		return fr
	}
	fr.Queued = true

	// Opportunistic fast path, strictly best-effort: failure here changes
	// nothing about correctness, since Drain will retry regardless (see this
	// function's doc comment, step 5).
	if resp, err := e.Client.PostNodeCreated(ctx, e.AgentID, payload); err == nil {
		if markErr := e.Queue.MarkNodeCreatedSubmitted(ctx, nodeUUID, resp.EventID, e.now()); markErr == nil {
			fr.SubmittedInline = true
		}
	}

	return fr
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
