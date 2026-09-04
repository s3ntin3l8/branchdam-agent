package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/naming"
	"github.com/s3ntin3l8/branchdam-agent/internal/phash"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// cHtimesFn is the indirection used to preserve source-mtime on
// destinations (issue #103's soft contract). The real call is os.Chtimes;
// tests substitute a stub that returns an error so they can assert the
// slog.Warn is emitted. Mirrors writer.go's syncParentDirFn pattern --
// the canonical "verify the log path without simulating a real fsync
// failure on tmpfs" trick this package uses for the equivalent
// dir-fsync log path.
var cHtimesFn = os.Chtimes

// preserveMtimeAt is a tiny wrapper around cHtimesFn that translates a
// failure into a slog.Warn carrying enough context to debug. Centralising
// the log shape here means every os.Chtimes call site in this package
// (the two in IngestCard's online path, the one in IngestCard's
// upload-stream path, the one in IngestCardOffline's offline path)
// emits an identical record -- source, destination, underlying error --
// so an operator looking for "why did my destination mtime not advance?"
// sees one message format no matter which code path got there.
func preserveMtimeAt(srcPath, dstPath string, mtime time.Time) {
	atime := time.Time{}
	if err := cHtimesFn(dstPath, atime, mtime); err != nil {
		slog.Warn("ingest: failed to preserve source mtime on destination",
			"source", srcPath,
			"destination", dstPath,
			"err", err,
		)
	}
}

// nodeCreator is the subset of *branchdam.Client's surface Engine needs, so
// tests can substitute a fake without a real HTTP server.
type nodeCreator interface {
	PostNodeCreated(ctx context.Context, agentID string, payload branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error)
}

// contentChecker is the subset of *branchdam.Client's surface Engine needs for
// content deduplication pre-flight checks.
type contentChecker interface {
	CheckContent(ctx context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error)
}

// uploader is the subset of *branchdam.Client's surface Engine needs for direct HTTP streaming ingest.
type uploader interface {
	Upload(ctx context.Context, body io.Reader, opts branchdam.UploadOptions) (*branchdam.UploadResponse, error)
}

// Engine drives one full ingest run over a card's contents: metadata
// extraction, the dual-copy verified write, DJI .srt GPS handling, and
// submission via the branchDAM agent-event client. Plain library code, no
// UI imports -- cmd/branchdam-agent's `ingest` subcommand and a later tray
// PR are both thin drivers over this type.
type Engine struct {
	Client   nodeCreator
	Uploader uploader
	AgentID  string
	Ingest   config.IngestConfig
	Mappings []config.PathMapping
	Exiftool *Exiftool

	// Queue backs IngestCardOffline (offline.go) -- nil for a plain online
	// Engine (IngestCard doesn't touch it), required for any call to
	// IngestCardOffline. Set directly by the caller (cmd/branchdam-agent's
	// -offline ingest path); NewEngine leaves it nil.
	Queue *queue.Store
	// Tier0ContainerRoot is the server-container path prefix
	// IngestCardOffline sends as EVENT_NODE_CREATED's filePath (the "Tier-0
	// container path" issue #4 requires) -- already in container-path form,
	// no PathMapping translation applied to it (contrast ArchivePath, which
	// is a workstation path translated via ToContainerPath). Required for
	// IngestCardOffline; unused by IngestCard.
	Tier0ContainerRoot string

	// Now/NewNodeUUID are overridable for tests; default to time.Now and a
	// real UUIDv7 mint (google/uuid.NewV7) via NewEngine.
	Now         func() time.Time
	NewNodeUUID func() (string, error)

	// Progress, if set, is called with byte-progress samples during a
	// file's copy and verify phases (DualWrite/WriteLocal's copy, Verify's
	// re-read). nil by default -- every existing caller (the headless
	// `ingest` subcommand, every test in this package) is unaffected; this
	// exists for a live "N of M bytes" readout (the tray's queue status,
	// issue #32), not something the ingest core itself needs.
	Progress func(ProgressEvent)
}

// progressOpts builds the WriteOption a DualWrite/WriteLocal/Verify call
// needs to report progress for path/phase/total through e.Progress -- nil
// (no options) when Progress itself is unset, so every call site stays a
// plain, zero-overhead call in the common case.
func (e *Engine) progressOpts(path string, phase ProgressPhase, total int64) []WriteOption {
	if e.Progress == nil {
		return nil
	}
	return []WriteOption{WithProgress(func(n int64) {
		e.Progress(ProgressEvent{Path: path, Phase: phase, BytesDone: n, TotalBytes: total})
	})}
}

// NewEngine builds an Engine with real clock/UUID/exiftool dependencies.
func NewEngine(client nodeCreator, agentID string, ingestCfg config.IngestConfig, mappings []config.PathMapping) *Engine {
	e := &Engine{
		Client:      client,
		AgentID:     agentID,
		Ingest:      ingestCfg,
		Mappings:    mappings,
		Exiftool:    NewExiftool(),
		Now:         time.Now,
		NewNodeUUID: func() (string, error) { id, err := uuid.NewV7(); return id.String(), err },
	}
	if u, ok := client.(uploader); ok {
		e.Uploader = u
	}
	return e
}

// FileResult is one card file's outcome.
type FileResult struct {
	SourcePath       string
	ArchivePath      string
	LocalPath        string
	IsSidecar        bool // .xmp/.srt -- copied to both destinations, no event submitted
	Skipped          bool
	SkipReason       string
	ExistingNodeUUID string
	NodeUUID         string
	EventID          string
	Write            WriteResult
	ArchiveVerify    VerifyResult
	LocalVerify      VerifyResult
	Exif             *ExifResult
	PHash            *int64
	GPSSource        string // "exif", "srt", or "" (no GPS)
	Err              error
}

// CardResult is one IngestCard call's full outcome.
type CardResult struct {
	Files []FileResult
}

// isLiveLifecycleState returns true if lifecycleState represents an active, live node.
// An empty string (default/unspecified) or "ACTIVE" is considered live. Non-live states
// (e.g. "ARCHIVED", "TRASHED", "DELETED") do not prevent ingest so content is not lost.
func isLiveLifecycleState(state string) bool {
	return state == "" || strings.EqualFold(state, "ACTIVE")
}

// IngestCard walks cardRoot (a mounted card's root directory, or the
// fixture directory a test points --card at) and ingests every regular
// file found under it. Returns per-file results even when some files
// failed -- a single bad file must not abort the rest of the card, matching
// the same "log and continue" spirit as branchDAM's own scan pipeline.
//
// Issue #100: the walk applies two pre-filters before any per-file
// pipeline (exiftool, DualWrite, EVENT_NODE_CREATED) runs:
//
//  1. by-name: Thumbs.db, System Volume Information, and any dotfile
//     (basename starting with "."). Case-insensitive on the named files.
//  2. by-extension: when Ingest.AllowedExtensions is non-empty, only
//     files whose extension is in that list are ingested (case-
//     insensitive, leading dot optional on either side). Files with
//     no extension are NOT filtered out by the allowlist; they fall
//     through to ingestFile so isImageExt/isVideoExt can positively
//     identify them.
//
// Filtered files appear in the result as FileResult{Skipped: true,
// SkipReason: "OS metadata: ..."} so an operator running
// `ingest --card <path>` and eyeballing res.Files can see what was
// rejected and why. They do NOT become media_nodes rows and do NOT
// trigger an exiftool subprocess.
//
// Partial-application semantics: ingestFile runs inline inside the
// WalkDir callback, so a mid-walk error (permission denied on a
// later directory, card yanked mid-scan, ...) propagates back as
// the returned err. By the time the walk errors, files already
// visited have already been DualWrite'd and submitted -- side
// effects are NOT rolled back. The function returns
// (CardResult{...}, err) where result.Files reflects whatever was
// processed before the error. The previous two-phase design
// (collect paths, then process after WalkDir returned) avoided
// this by collecting no side effects during the walk, at the
// cost of holding every path in memory and re-walking the dir's
// metadata twice. The current design favors throughput over
// atomicity: re-running on the same card is always safe (the
// queue.db BySourcePath check in offline's twin path; the
// AlreadyIngested fast-path in this one), so a partial run
// followed by a re-run is a known-good recovery.
func (e *Engine) IngestCard(ctx context.Context, cardRoot string) (CardResult, error) {
	stemSuffix := make(map[string]string)
	var dedupUnavailable bool
	var result CardResult
	err := filepath.WalkDir(cardRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if skip, reason := shouldSkipByName(path); skip {
			result.Files = append(result.Files, FileResult{
				SourcePath: path,
				Skipped:    true,
				SkipReason: reason,
			})
			return nil
		}
		if shouldSkipByExtension(e.Ingest.AllowedExtensions, extNoDot(path)) {
			result.Files = append(result.Files, FileResult{
				SourcePath: path,
				Skipped:    true,
				SkipReason: fmt.Sprintf("extension %q not in allowedExtensions", extNoDot(path)),
			})
			return nil
		}
		result.Files = append(result.Files, e.ingestFile(ctx, path, stemSuffix, &dedupUnavailable))
		return nil
	})
	if err != nil {
		return CardResult{}, fmt.Errorf("ingest: walk card root %s: %w", cardRoot, err)
	}
	return result, nil
}

// ingestFile runs the full pipeline for one source file: metadata
// extraction (needed up front to fill the naming template), dual-copy
// write, verify, DJI .srt GPS, and (for non-sidecar files) submission.
func (e *Engine) ingestFile(ctx context.Context, srcPath string, stemSuffix map[string]string, dedupUnavailable *bool) FileResult {
	if e.Ingest.UploadStream {
		return e.ingestFileUpload(ctx, srcPath)
	}

	fr := FileResult{SourcePath: srcPath}

	ext := extNoDot(srcPath)
	fr.IsSidecar = ext == "xmp" || ext == "srt"

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		fr.Err = fmt.Errorf("stat source: %w", err)
		return fr
	}

	// Metadata extraction runs against the SOURCE path, before the copy:
	// the naming template needs capturedAt/cameraModel to build the
	// destination path, and exiftool's own read of the card is a separate,
	// necessary subprocess invocation -- distinct from the one Go-level
	// byte-stream read DualWrite performs for hashing, which is the "one
	// read" issue #2's contract is actually about (avoiding a second
	// multi-hundred-MB pass over a slow card reader for hashing purposes).
	var exif *ExifResult
	if e.Exiftool != nil && e.Exiftool.HasExiftool() && !fr.IsSidecar {
		if res, err := e.Exiftool.Exif(ctx, srcPath); err == nil {
			exif = res
		}
	}
	fr.Exif = exif

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

	roots := []string{e.Ingest.ArchiveRoot, e.Ingest.LocalEditRoot}
	resolution := ResolveDestination(roots, e.Ingest.PathTemplate, vars, srcPath, knownSuffix)
	if stemSuffix != nil && resolution.Suffix != "" {
		stemSuffix[stemKey] = resolution.Suffix
	}

	relPath := resolution.RelPath
	archivePath := filepath.Join(e.Ingest.ArchiveRoot, relPath)
	localPath := filepath.Join(e.Ingest.LocalEditRoot, relPath)
	fr.ArchivePath = archivePath
	fr.LocalPath = localPath

	if resolution.AlreadyIngested {
		fr.Skipped = true
		fr.SkipReason = "already ingested (identical file exists at destination)"
		return fr
	}

	// Pre-flight BLAKE3 content dedup check before DualWrite.
	// When enabled, queries GET /api/v1/agent/check-content first with fastHash
	// (xxHash64 sampled via fastHashFile), and on a hit confirms with fullHash
	// (BLAKE3-256) to skip writing duplicate bytes to archiveRoot / localEditRoot.
	if (dedupUnavailable == nil || !*dedupUnavailable) && e.Ingest.PreflightTimeoutSecs >= 0 && e.Client != nil && !fr.IsSidecar {
		if checker, ok := e.Client.(contentChecker); ok {
			timeout := time.Duration(config.DefaultPreflightTimeoutSecs) * time.Second
			if e.Ingest.PreflightTimeoutSecs > 0 {
				timeout = time.Duration(e.Ingest.PreflightTimeoutSecs) * time.Second
			}

			fastHash, err := fastHashFile(srcPath, srcInfo.Size())
			if err != nil {
				slog.Warn("ingest: pre-flight fast hash failed", "source", srcPath, "err", err)
			} else {
				pCtx, pCancel := context.WithTimeout(ctx, timeout)
				res, err := checker.CheckContent(pCtx, fastHash, "")
				pCancel()
				if err != nil {
					slog.Warn("ingest: content check pre-flight failed (fail-open)", "source", srcPath, "err", err)
					if dedupUnavailable != nil {
						var he *branchdam.HTTPError
						if !errors.As(err, &he) || (he.StatusCode == http.StatusNotFound || he.StatusCode == http.StatusNotImplemented) {
							*dedupUnavailable = true
						}
					}
				} else if res.Found {
					fullHash, err := blake3File(srcPath)
					if err != nil {
						slog.Warn("ingest: pre-flight full hash failed", "source", srcPath, "err", err)
					} else {
						pCtx2, pCancel2 := context.WithTimeout(ctx, timeout)
						res2, err := checker.CheckContent(pCtx2, fastHash, fullHash)
						pCancel2()
						if err != nil {
							slog.Warn("ingest: content check pre-flight confirmation failed (fail-open)", "source", srcPath, "err", err)
							if dedupUnavailable != nil {
								var he *branchdam.HTTPError
								if !errors.As(err, &he) || (he.StatusCode == http.StatusNotFound || he.StatusCode == http.StatusNotImplemented) {
									*dedupUnavailable = true
								}
							}
						} else if res2.Found && isLiveLifecycleState(res2.LifecycleState) {
							fr.Skipped = true
							fr.SkipReason = fmt.Sprintf("duplicate: already in library as node %s at %s", res2.NodeUUID, res2.FilePath)
							fr.ExistingNodeUUID = res2.NodeUUID
							return fr
						}
					}
				}
			}
		}
	}

	writeRes, err := DualWrite(srcPath, archivePath, localPath, e.progressOpts(localPath, ProgressPhaseCopying, srcInfo.Size())...)
	if err != nil {
		fr.Err = fmt.Errorf("dual write: %w", err)
		return fr
	}
	fr.Write = writeRes

	// Preserve the source's original mtime on both destinations -- an
	// ingested master's mtime should reflect when it was captured/written
	// on the card, not the moment this agent happened to copy it.
	//
	// Soft contract (issue #103): a Chtimes failure here is logged, not
	// fatal -- the file is on disk and verified, only its mtime
	// preservation is best-effort. But the failure MUST be surfaced
	// (slog.Warn), because the prune-safety half of invariant #8 depends
	// on the destination mtime advancing past the source's, and a silent
	// Chtimes failure is exactly the case prune silently stops deleting.
	preserveMtimeAt(srcPath, archivePath, srcInfo.ModTime())
	preserveMtimeAt(srcPath, localPath, srcInfo.ModTime())

	archiveVerify, err := Verify(archivePath, writeRes.FullHash, e.progressOpts(archivePath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
	if err != nil {
		_ = os.Remove(archivePath)
		_ = os.Remove(localPath)
		fr.Err = fmt.Errorf("verify archive copy: %w", err)
		return fr
	}
	fr.ArchiveVerify = archiveVerify
	localVerify, err := Verify(localPath, writeRes.FullHash, e.progressOpts(localPath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
	if err != nil {
		_ = os.Remove(archivePath)
		_ = os.Remove(localPath)
		fr.Err = fmt.Errorf("verify local copy: %w", err)
		return fr
	}
	fr.LocalVerify = localVerify

	if !archiveVerify.Verified || !localVerify.Verified {
		_ = os.Remove(archivePath)
		_ = os.Remove(localPath)
		fr.Err = fmt.Errorf("ingest: verification failed for %s (archive verified=%v, local verified=%v) -- safe-eject withheld", srcPath, archiveVerify.Verified, localVerify.Verified)
		return fr
	}

	if e.Ingest.RequireUnbuffered && (archiveVerify.Method == VerifyMethodBufferedFloor || localVerify.Method == VerifyMethodBufferedFloor) {
		fr.Err = fmt.Errorf("ingest: unbuffered verify required by config, but verify fell back to buffered floor (archive=%s, local=%s) -- safe-eject withheld", archiveVerify.Method, localVerify.Method)
		return fr
	}

	if fr.IsSidecar {
		fr.Skipped = true
		fr.SkipReason = "sidecar file (.xmp/.srt): copied to both destinations, no EVENT_NODE_CREATED submitted"
		return fr
	}

	// pHash is computed against the just-written LOCAL copy, not the
	// source: content is guaranteed byte-identical to the source (that is
	// exactly what Verify proved), and reading local disk for the
	// (potentially several) decode/preview-extraction attempts avoids a
	// second hit against a slow card reader.
	if e.Exiftool != nil && isImageExt(ext) {
		if ph, err := phash.Extract(ctx, e.Exiftool.Path(), localPath); err == nil {
			fr.PHash = ph
		}
	}

	gpsLat, gpsLon := exifGPS(exif)
	gpsSource := ""
	if gpsLat != nil {
		gpsSource = "exif"
	} else if isVideoExt(ext) {
		if srtPath, ok := findSRTSidecar(srcPath); ok {
			if lat, lon, ok, err := srtGPS(srtPath); err == nil && ok {
				gpsLat, gpsLon = &lat, &lon
				gpsSource = "srt"
			}
		}
	}
	fr.GPSSource = gpsSource

	nodeUUID, err := e.NewNodeUUID()
	if err != nil {
		fr.Err = fmt.Errorf("mint node uuid: %w", err)
		return fr
	}
	fr.NodeUUID = nodeUUID

	containerPath, err := ToContainerPath(e.Mappings, archivePath)
	if err != nil {
		fr.Err = err
		return fr
	}

	payload := branchdam.NodeCreatedPayload{
		NodeUUID:     nodeUUID,
		FilePath:     containerPath,
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

	resp, err := e.Client.PostNodeCreated(ctx, e.AgentID, payload)
	if err != nil {
		fr.Err = fmt.Errorf("submit EVENT_NODE_CREATED: %w", err)
		return fr
	}
	fr.EventID = resp.EventID
	return fr
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func exifGPS(exif *ExifResult) (*float64, *float64) {
	if exif == nil {
		return nil, nil
	}
	return exif.GPSLatitude, exif.GPSLongitude
}

// strPtr returns nil for an empty string, matching NodeCreatedPayload's
// *string-omitempty fields (nil means "not sent", not "sent as empty").
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ingestFileUpload streams media directly to POST /api/v1/agent/upload on the
// branchDAM server, persists the local edit copy under LocalEditRoot with the
// server-returned relativePath, and verifies cryptographic BLAKE3 parity.
func (e *Engine) ingestFileUpload(ctx context.Context, srcPath string) FileResult {
	fr := FileResult{SourcePath: srcPath}

	ext := extNoDot(srcPath)
	fr.IsSidecar = ext == "xmp" || ext == "srt"

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
	fr.Exif = exif

	var captureTimestamp int64
	var cameraModel string
	if exif != nil && exif.CapturedAt != nil {
		captureTimestamp = exif.CapturedAt.Unix()
	} else {
		captureTimestamp = srcInfo.ModTime().Unix()
	}
	if exif != nil {
		cameraModel = exif.CameraModel
	}

	if e.Uploader == nil {
		fr.Err = fmt.Errorf("ingest: uploadStream is true but client does not support upload")
		return fr
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		fr.Err = fmt.Errorf("open source %s: %w", srcPath, err)
		return fr
	}
	defer func() { _ = srcFile.Close() }()

	uploadOpts := branchdam.UploadOptions{
		Filename:         filepath.Base(srcPath),
		CameraModel:      cameraModel,
		CaptureTimestamp: captureTimestamp,
	}

	var body io.Reader = srcFile
	if o := applyWriteOptions(e.progressOpts(srcPath, ProgressPhaseCopying, srcInfo.Size())); o.onBytes != nil {
		body = &progressReader{r: srcFile, onBytes: o.onBytes}
	}

	upResp, err := e.Uploader.Upload(ctx, body, uploadOpts)
	if err != nil {
		fr.Err = fmt.Errorf("upload %s: %w", srcPath, err)
		return fr
	}

	fr.NodeUUID = upResp.NodeUUID
	fr.ArchivePath = upResp.RelativePath
	localPath := filepath.Join(e.Ingest.LocalEditRoot, upResp.RelativePath)
	fr.LocalPath = localPath

	writeRes, err := WriteLocal(srcPath, localPath, e.progressOpts(localPath, ProgressPhaseCopying, srcInfo.Size())...)
	if err != nil {
		fr.Err = fmt.Errorf("write local copy: %w", err)
		return fr
	}
	fr.Write = writeRes
	// Soft contract (issue #103): the archive landed server-side via
	// Upload, so the only Chtimes call on this path is the local edit
	// copy. Failure is logged, not fatal -- see preserveMtimeAt.
	preserveMtimeAt(srcPath, localPath, srcInfo.ModTime())

	localVerify, err := Verify(localPath, upResp.Blake3Hash, e.progressOpts(localPath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
	if err != nil {
		_ = os.Remove(localPath)
		fr.Err = fmt.Errorf("verify local copy: %w", err)
		return fr
	}
	fr.LocalVerify = localVerify

	if !localVerify.Verified {
		_ = os.Remove(localPath)
		fr.Err = fmt.Errorf("ingest: verification failed for %s (local verified=%v, server blake3=%s, local blake3=%s) -- safe-eject withheld",
			srcPath, localVerify.Verified, upResp.Blake3Hash, writeRes.FullHash)
		return fr
	}

	if e.Ingest.RequireUnbuffered && localVerify.Method == VerifyMethodBufferedFloor {
		fr.Err = fmt.Errorf("ingest: unbuffered verify required by config, but verify fell back to buffered floor (local=%s) -- safe-eject withheld", localVerify.Method)
		return fr
	}

	if isImageExt(ext) && e.Exiftool != nil {
		if ph, err := phash.Extract(ctx, e.Exiftool.Path(), localPath); err == nil {
			fr.PHash = ph
		}
	}

	return fr
}

// blake3File computes the BLAKE3-256 hex digest of the file at p in a single pass.
func blake3File(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // path is our source file
	if err != nil {
		return "", fmt.Errorf("open for blake3: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash blake3: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
