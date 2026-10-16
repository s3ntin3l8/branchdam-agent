package ingest

import (
	"context"
	"encoding/json"
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

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/naming"
	"github.com/s3ntin3l8/branchdam-agent/internal/netgate"
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
	PostNodeCreatedWithUUID(ctx context.Context, agentID string, payload branchdam.NodeCreatedPayload, eventUUID string) (*branchdam.EventResponse, error)
}

// contentChecker is the subset of *branchdam.Client's surface Engine needs for
// content deduplication pre-flight checks.
type contentChecker interface {
	CheckContent(ctx context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error)
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

	// Now/NewNodeUUID/MintEventUUID are overridable for tests; default to time.Now, a
	// real UUIDv7 mint (google/uuid.NewV7 with v4 fallback) and branchdam.MintEventUUID via NewEngine.
	Now           func() time.Time
	NewNodeUUID   func() (string, error)
	MintEventUUID func() string
	// IsMetered is overridable for tests; default to netgate.IsMetered via NewEngine.
	IsMetered func() (bool, error)
	// ArchiveReachable is overridable for tests: IngestCardOffline's probe
	// for whether Ingest.ArchiveRoot can be checked for name collisions.
	// nil means a stat of archiveRoot bounded by archiveProbeTimeout.
	ArchiveReachable func(ctx context.Context, archiveRoot string) bool

	// Progress, if set, is called with byte-progress samples during a
	// file's copy and verify phases (DualWrite/WriteLocal's copy, Verify's
	// re-read). nil by default -- every existing caller (the headless
	// `ingest` subcommand, every test in this package) is unaffected; this
	// exists for a live "N of M bytes" readout (the tray's queue status,
	// issue #32), not something the ingest core itself needs.
	Progress func(ProgressEvent)
}

func (e *Engine) mintEventUUID() string {
	if e != nil && e.MintEventUUID != nil {
		return e.MintEventUUID()
	}
	return branchdam.MintEventUUID()
}

// progressOpts builds the WriteOptions every DualWrite/WriteLocal/Verify
// call in this package passes: WithContext(ctx), so a cancelled ingest
// stops within one buffer of the copy or verify it is in, and -- when
// e.Progress is set -- WithProgress reporting path/phase/total through it.
func (e *Engine) progressOpts(ctx context.Context, path string, phase ProgressPhase, total int64) []WriteOption {
	opts := []WriteOption{WithContext(ctx)}
	if e.Progress == nil {
		return opts
	}
	return append(opts, WithProgress(func(n int64) {
		e.Progress(ProgressEvent{Path: path, Phase: phase, BytesDone: n, TotalBytes: total})
	}))
}

// NewEngine builds an Engine with real clock/UUID/exiftool dependencies.
func NewEngine(client nodeCreator, agentID string, ingestCfg config.IngestConfig, mappings []config.PathMapping) *Engine {
	e := &Engine{
		Client:        client,
		AgentID:       agentID,
		Ingest:        ingestCfg,
		Mappings:      mappings,
		Exiftool:      NewExiftoolAt(ingestCfg.ExiftoolPath),
		Now:           time.Now,
		MintEventUUID: branchdam.MintEventUUID,
		NewNodeUUID: func() (string, error) {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New().String(), nil
			}
			return id.String(), nil
		},
		IsMetered: netgate.IsMetered,
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
	// Deferred is set (alongside Skipped) when uploadStream mode held the
	// upload back on a metered connection: the local edit copy landed and
	// verified, but nothing reached branchDAM or the archive, so the card
	// is still the only copy outside this workstation. Callers must treat
	// a Deferred file as unfinished -- never "safe to eject" -- even though
	// it carries no Err.
	Deferred bool
	// Queued is set when both copies landed and verified but
	// EVENT_NODE_CREATED could not be submitted during this run (server
	// down, request failed, ingest cancelled): the registration is
	// durably recorded in queue.db and Drain submits it. The file is safe
	// -- archived and verified -- so this is not a failure, and EventID is
	// empty. Only possible when Engine.Queue is set; without a queue a
	// failed submission is an Err.
	Queued bool
	Err    error
}

// CardResult is one IngestCard call's full outcome.
type CardResult struct {
	Files []FileResult
}

// isLiveLifecycleState returns true if lifecycleState represents a live node
// -- the same set branchDAM's check-content pre-screen counts: ACTIVE and
// HIDDEN (a hidden node is still in the library, just out of the default
// views). An empty string (unspecified) is treated as live too. Other states
// (ARCHIVED, MISSING, TRASHED, ...) do not prevent ingest, so content is not
// lost.
func isLiveLifecycleState(state string) bool {
	return state == "" || strings.EqualFold(state, "ACTIVE") || strings.EqualFold(state, "HIDDEN")
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
// OS metadata directories (.Trashes, .Spotlight-V100, .fseventsd,
// System Volume Information, $RECYCLE.BIN, any dot-directory -- see
// shouldSkipDir) are not descended at all; each appears once in the
// result as a Skipped entry naming the directory.
//
// Partial-application semantics: ingestFile runs inline inside the
// WalkDir callback, so by the time anything goes wrong mid-walk, files
// already visited have already been DualWrite'd and submitted -- side
// effects are NOT rolled back. An unreadable entry below cardRoot
// (permission denied on a subdirectory, a flaky card reader) is
// therefore recorded as a failed FileResult and the walk continues, so
// the result keeps every file already processed and still covers the
// rest of the card; the failed entry is what keeps the run from reading
// OK. Only an error on cardRoot itself returns (CardResult{}, err).
// A cancelled ctx stops the walk before the next file (and
// DualWrite/Verify within one buffer of the file in flight); IngestCard
// then returns the files already processed together with ctx's error.
// Re-running on the same card is always safe (the AlreadyIngested
// fast-path here; queue.db's BySourcePath check in the offline twin).
func (e *Engine) IngestCard(ctx context.Context, cardRoot string) (CardResult, error) {
	stemSuffix := make(map[string]string)
	var dedup dedupState
	var result CardResult
	var upload *uploadPass
	if e.Ingest.UploadStream {
		upload = newUploadPass()
	}
	err := filepath.WalkDir(cardRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			entryErr, fatal := walkEntryError(cardRoot, path, err)
			if entryErr != nil {
				result.Files = append(result.Files, FileResult{SourcePath: path, Err: entryErr})
			}
			return fatal
		}
		if d.IsDir() {
			if skip, reason := shouldSkipDir(cardRoot, path); skip {
				result.Files = append(result.Files, FileResult{SourcePath: path, Skipped: true, SkipReason: reason})
				return filepath.SkipDir
			}
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
		if err := ctx.Err(); err != nil {
			return err
		}
		if upload != nil {
			// uploadStream mode (upload.go): sidecars wait for every
			// master so they can follow it. checkContentDedup does not
			// run: the server dedups by the X-Blake3-Hash the upload
			// carries, computed in flight from the single card read.
			if isSidecarExt(extNoDot(path)) {
				upload.sidecars = append(upload.sidecars, path)
				return nil
			}
			result.Files = append(result.Files, e.ingestFileUpload(ctx, path, upload))
			return nil
		}
		result.Files = append(result.Files, e.ingestFile(ctx, path, stemSuffix, &dedup))
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return result, fmt.Errorf("ingest: card %s: stopped before the remaining files: %w", cardRoot, err)
		}
		return CardResult{}, fmt.Errorf("ingest: walk card root %s: %w", cardRoot, err)
	}
	if upload != nil {
		sidecars, err := e.uploadSidecars(ctx, upload)
		result.Files = append(result.Files, sidecars...)
		if err != nil {
			return result, fmt.Errorf("ingest: card %s: stopped before the remaining sidecars: %w", cardRoot, err)
		}
	}
	return result, nil
}

// ingestFile runs the full pipeline for one source file: metadata
// extraction (needed up front to fill the naming template), dual-copy
// write, content dedup, verify, DJI .srt GPS, and (for non-sidecar files)
// registration.
//
// Ordering matters for what a failure leaves behind. Everything that can
// fail without touching the destinations -- naming, the archive path's
// container mapping -- runs before DualWrite. The content dedup needs the
// hashes DualWrite computes in flight (the card is read once), so it runs
// right after the copy; a confirmed duplicate removes both copies this
// call just created. A failed verify (or an unbuffered verify the config
// requires but didn't get) removes them too. Once both copies are
// verified the file is registered by registerNode, which records the
// intent in queue.db before the POST. And a later run that finds the
// copies already in place does not take that as proof of registration
// (confirmRegistered).
func (e *Engine) ingestFile(ctx context.Context, srcPath string, stemSuffix map[string]string, dedup *dedupState) FileResult {
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

	vars := buildTemplateVars(exif, srcPath, srcInfo.ModTime())

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
		if fr.IsSidecar {
			fr.Skipped = true
			fr.SkipReason = alreadyIngestedReason
			return fr
		}
		return e.confirmRegistered(ctx, fr, srcInfo, exif, dedup)
	}

	// Map the archive path to its container path before anything is
	// written: a missing or wrong pathMappings entry must fail the file
	// while both destinations are still empty, not after the copy, when it
	// would leave two verified files nothing ever registered.
	var containerPath string
	if !fr.IsSidecar {
		cp, err := ToContainerPath(e.Mappings, archivePath)
		if err != nil {
			fr.Err = err
			return fr
		}
		containerPath = cp
	}

	writeRes, err := DualWrite(srcPath, archivePath, localPath, e.progressOpts(ctx, localPath, ProgressPhaseCopying, srcInfo.Size())...)
	if err != nil {
		fr.Err = fmt.Errorf("dual write: %w", err)
		return fr
	}
	fr.Write = writeRes

	// Content dedup against the library, using the hashes DualWrite just
	// computed in flight: the card is read once (invariant 1), never
	// re-read to hash it for this check. A confirmed live duplicate means
	// the two copies DualWrite itself just created (O_EXCL, so they are
	// ours) are removed and nothing is registered. It runs before Verify,
	// so a duplicate costs no verify pass either.
	// containerPath (mapped above, before the copy) is this file's own
	// archive container path: a match there is the copy just written.
	if !fr.IsSidecar {
		if skip, existingUUID, skipReason := e.checkContentDedup(ctx, srcPath, writeRes, containerPath, config.DefaultPreflightTimeoutSecs, dedup); skip {
			// A copy that cannot be removed is a redundant, unregistered
			// copy nothing will ever clean up (a later run skips it on the
			// existing node), so say so in the result instead of reporting
			// a clean skip.
			for _, p := range []string{archivePath, localPath} {
				if rmErr := removeDuplicateCopy(srcPath, p); rmErr != nil {
					skipReason += fmt.Sprintf("; this run's copy at %s was left behind (%v) -- delete it by hand", p, rmErr)
				}
			}
			fr.Skipped = true
			fr.SkipReason = skipReason
			fr.ExistingNodeUUID = existingUUID
			return fr
		}
	}

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

	archiveVerify, err := Verify(archivePath, writeRes.FullHash, e.progressOpts(ctx, archivePath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
	if err != nil {
		_ = os.Remove(archivePath)
		_ = os.Remove(localPath)
		fr.Err = fmt.Errorf("verify archive copy: %w", err)
		return fr
	}
	fr.ArchiveVerify = archiveVerify
	localVerify, err := Verify(localPath, writeRes.FullHash, e.progressOpts(ctx, localPath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
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
		// Both files were created by this call's DualWrite (O_EXCL) and
		// are not registered. Leaving them would let the next run find
		// them as already ingested and pass them without the unbuffered
		// verify the operator asked for; removing them means the next run
		// copies again and meets this same check.
		_ = os.Remove(archivePath)
		_ = os.Remove(localPath)
		fr.Err = fmt.Errorf("ingest: unbuffered verify required by config, but verify fell back to buffered floor (archive=%s, local=%s) -- copies removed, safe-eject withheld", archiveVerify.Method, localVerify.Method)
		return fr
	}

	if fr.IsSidecar {
		fr.Skipped = true
		fr.SkipReason = "sidecar file (.xmp/.srt): copied to both destinations, no EVENT_NODE_CREATED submitted"
		return fr
	}

	e.registerNode(ctx, &fr, srcInfo, exif, containerPath, writeRes)
	return fr
}

// alreadyIngestedReason is the SkipReason prefix for a file whose
// identical copies are already at both destinations.
const alreadyIngestedReason = "already ingested (identical file exists at destination)"

// registerNode registers a file whose two copies are in place at
// fr.ArchivePath/fr.LocalPath and verified against w.FullHash: it builds
// EVENT_NODE_CREATED (pHash from the local copy, GPS from EXIF or a DJI
// .srt sidecar), records the intent in queue.db when e.Queue is set, and
// only then POSTs it (invariant 3). A POST that fails leaves the row
// PENDING for Drain to retry and marks fr.Queued; it is an Err only when
// there is no queue to fall back on. The queue writes ignore ctx's
// cancellation: the copies are already in place, and recording that is
// what lets a cancelled ingest be finished later instead of forgotten.
func (e *Engine) registerNode(ctx context.Context, fr *FileResult, srcInfo os.FileInfo, exif *ExifResult, containerPath string, w WriteResult) {
	srcPath, archivePath, localPath := fr.SourcePath, fr.ArchivePath, fr.LocalPath
	ext := extNoDot(srcPath)

	// pHash is computed against the just-written LOCAL copy, not the
	// source: content is guaranteed byte-identical to the source (that is
	// exactly what Verify proved), and reading local disk for the
	// (potentially several) decode/preview-extraction attempts avoids a
	// second hit against a slow card reader.
	if e.Exiftool != nil && isImageExt(ext) {
		if ph, err := phash.Extract(ctx, e.Exiftool.Pool(), localPath); err == nil {
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
		return
	}
	fr.NodeUUID = nodeUUID

	payload := branchdam.NodeCreatedPayload{
		NodeUUID:     nodeUUID,
		FilePath:     containerPath,
		FileName:     filepath.Base(archivePath),
		FileExt:      ext,
		SizeBytes:    w.SizeBytes,
		MtimeUnix:    srcInfo.ModTime().Unix(),
		FastHash:     &w.FastHash,
		FullHash:     &w.FullHash,
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

	eventUUID := e.mintEventUUID()
	durable := context.WithoutCancel(ctx)
	if e.Queue != nil {
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			fr.Err = fmt.Errorf("marshal node created payload: %w", err)
			return
		}
		rec := queue.NewRecord{
			NodeUUID:               nodeUUID,
			Kind:                   queue.KindMedia,
			Origin:                 queue.OriginOnline,
			SourcePath:             srcPath,
			LocalPath:              localPath,
			ArchivePath:            archivePath,
			ArchiveContainerPath:   containerPath,
			Tier0ContainerPath:     containerPath,
			FileName:               filepath.Base(archivePath),
			FileExt:                ext,
			SizeBytes:              w.SizeBytes,
			MtimeUnix:              srcInfo.ModTime().Unix(),
			FullHash:               w.FullHash,
			FastHash:               w.FastHash,
			NodeCreatedPayloadJSON: string(payloadJSON),
			NodeCreatedEventUUID:   eventUUID,
		}
		if err := e.Queue.InsertPending(durable, rec); err != nil {
			// Nothing was promised to the server yet. The copies stay;
			// a re-run finds them and registers them (confirmRegistered).
			fr.Err = fmt.Errorf("record ingest in queue.db before submitting: %w", err)
			return
		}
	}

	resp, err := e.Client.PostNodeCreatedWithUUID(ctx, e.AgentID, payload, eventUUID)
	if err != nil {
		if e.Queue == nil {
			fr.Err = fmt.Errorf("submit EVENT_NODE_CREATED: %w", err)
			return
		}
		if markErr := e.Queue.MarkNodeCreatedAttempt(durable, nodeUUID, err.Error(), backoffFor(1, e.now())); markErr != nil {
			slog.Warn("ingest: could not record the failed EVENT_NODE_CREATED attempt; the row stays pending and Drain retries it", "node", nodeUUID, "err", markErr)
		}
		slog.Warn("ingest: EVENT_NODE_CREATED not accepted; queued for Drain", "source", srcPath, "node", nodeUUID, "err", err)
		fr.Queued = true
		return
	}
	fr.EventID = resp.EventID
	if e.Queue != nil {
		if err := e.Queue.MarkNodeCreatedSubmitted(durable, nodeUUID, resp.EventID, e.now()); err != nil {
			// The server has it. Drain re-sends the same nodeUuid and
			// event UUID, which the server treats as a no-op.
			slog.Warn("ingest: could not mark EVENT_NODE_CREATED submitted in queue.db; Drain will re-send it harmlessly", "node", nodeUUID, "err", err)
		}
	}
}

// confirmRegistered handles a file whose identical copies already sit at
// both destinations (ResolveDestination's AlreadyIngested). Finding the
// bytes there proves an earlier run copied them -- not that it registered
// them: that run may have died, been cancelled, failed its POST, or
// stopped at a buffered verify the config forbids. Skipping on the files
// alone is how a copied-but-unregistered file used to be skipped on every
// later run, so the skip needs evidence of registration, cheapest first:
//
//  1. A queue.db row for this exact destination (online ingest records one
//     before its POST): the registration was accepted, or is queued and
//     Drain is retrying it. Skip.
//  2. Otherwise hash the local copy (a cache-defeating read of the SSD,
//     not the card) and ask the server's check-content by that full hash
//     (serverHasLiveContent; no pre-screen, the full hash is in hand). A
//     live node (ACTIVE or HIDDEN) with this content: skip -- reported as
//     this file's registration when the node is at this file's own
//     archive container path, otherwise as the server's existing node.
//  3. Otherwise -- not found, or the server can't say -- register it now:
//     verify the archive copy against that hash, apply RequireUnbuffered to
//     both reads, then registerNode as for a fresh copy. Registering
//     content the server already has is harmless: it drops an
//     EVENT_NODE_CREATED whose full_hash a live node already carries.
//
// Nothing here deletes or rewrites the existing copies: they are not this
// run's files. "Identical" is ResolveDestination's sampled FastHash match
// against the card, the same evidence the old unconditional skip relied
// on; the hash registered is that of the copies themselves.
func (e *Engine) confirmRegistered(ctx context.Context, fr FileResult, srcInfo os.FileInfo, exif *ExifResult, dedup *dedupState) FileResult {
	archivePath, localPath := fr.ArchivePath, fr.LocalPath

	if e.Queue != nil {
		rec, found, err := e.Queue.ByLocalPath(ctx, localPath)
		if err != nil {
			// Fail closed: without the lookup a skip would be a guess.
			fr.Err = fmt.Errorf("queue lookup for already-present copy %s: %w", localPath, err)
			return fr
		}
		if found && rec.ArchivePath == archivePath && rec.SizeBytes == srcInfo.Size() && !rec.Failed() {
			fr.Skipped = true
			fr.NodeUUID = rec.NodeUUID
			if rec.NodeCreatedStatus == queue.StatusPending {
				fr.SkipReason = fmt.Sprintf("%s; registration of node %s is queued and retried by the queue drain", alreadyIngestedReason, rec.NodeUUID)
			} else {
				fr.SkipReason = fmt.Sprintf("%s; registered as node %s", alreadyIngestedReason, rec.NodeUUID)
			}
			return fr
		}
	}

	size := srcInfo.Size()
	localVerify, err := Verify(localPath, "", e.progressOpts(ctx, localPath, ProgressPhaseVerifying, size)...)
	if err != nil {
		fr.Err = fmt.Errorf("hash already-present local copy: %w", err)
		return fr
	}
	// This read is the reference the archive copy is checked against.
	localVerify.Verified = true
	fullHash := localVerify.Hash
	fastHash, err := fastHashFile(localPath, size)
	if err != nil {
		fr.Err = fmt.Errorf("fast hash already-present local copy: %w", err)
		return fr
	}

	// Mapped up front so the server's answer can be told apart: a live node
	// at this file's own archive container path is this very copy, already
	// registered (an earlier run's registration, or the server's own scan
	// of the archive) -- evidence of registration, never a duplicate. A
	// mapping error only matters if the copies get registered below.
	containerPath, containerErr := ToContainerPath(e.Mappings, archivePath)

	if res, found := e.serverHasLiveContent(ctx, fr.SourcePath, fastHash, size, fullHash, dedup); found {
		fr.Skipped = true
		if containerErr == nil && res.FilePath == containerPath {
			fr.NodeUUID = res.NodeUUID
			fr.SkipReason = fmt.Sprintf("%s; registered as node %s", alreadyIngestedReason, res.NodeUUID)
		} else {
			fr.ExistingNodeUUID = res.NodeUUID
			fr.SkipReason = fmt.Sprintf("%s; server already has it as node %s", alreadyIngestedReason, res.NodeUUID)
		}
		return fr
	}

	archiveVerify, err := Verify(archivePath, fullHash, e.progressOpts(ctx, archivePath, ProgressPhaseVerifying, size)...)
	if err != nil {
		fr.Err = fmt.Errorf("verify already-present archive copy: %w", err)
		return fr
	}
	fr.ArchiveVerify = archiveVerify
	fr.LocalVerify = localVerify
	if !archiveVerify.Verified {
		fr.Err = fmt.Errorf("ingest: %s and %s are both present but differ (archive BLAKE3 %s, local %s) -- neither registered nor changed; resolve by hand, safe-eject withheld",
			archivePath, localPath, archiveVerify.Hash, fullHash)
		return fr
	}
	if e.Ingest.RequireUnbuffered && (archiveVerify.Method == VerifyMethodBufferedFloor || localVerify.Method == VerifyMethodBufferedFloor) {
		fr.Err = fmt.Errorf("ingest: copies already at %s are not registered, and unbuffered verify is required by config but fell back to buffered floor (archive=%s, local=%s) -- not registering, safe-eject withheld", archivePath, archiveVerify.Method, localVerify.Method)
		return fr
	}

	if containerErr != nil {
		fr.Err = containerErr
		return fr
	}
	fr.Write = WriteResult{ArchivePath: archivePath, LocalPath: localPath, SizeBytes: size, FullHash: fullHash, FastHash: fastHash}
	slog.Info("ingest: copies already at destination are not known to be registered -- registering them", "source", fr.SourcePath, "archive", archivePath)
	e.registerNode(ctx, &fr, srcInfo, exif, containerPath, fr.Write)
	return fr
}

// serverHasLiveContent asks GET /api/v1/agent/check-content whether a
// live node (ACTIVE or HIDDEN) already has the content of an
// already-present local copy -- confirmRegistered's step 2. The full hash
// is already in hand, so there is no pre-screen: it goes straight to the
// definitive full-hash lookup, sending fastHash and sizeBytes alongside
// when known. The per-pass dedupState rules are checkContentDedup's: a
// 400 means a server predating the pre-screen, so the lookup is retried
// once in the form such servers accept (fastHash + fullHash) and
// dedup.fullOnly makes the rest of the pass use that form; a failure
// noteError latches (transport error, 404/501, 401/403, 429) sets
// dedup.unavailable for the rest of the pass.
// found=false covers "not there" and "couldn't ask" alike (no checker,
// pre-flight disabled, unavailable, or an error) -- the caller then
// registers, which the server dedups by full_hash if it does have it.
func (e *Engine) serverHasLiveContent(ctx context.Context, srcPath, fastHash string, size int64, fullHash string, dedup *dedupState) (res branchdam.ContentCheckResult, found bool) {
	if dedup == nil {
		dedup = &dedupState{}
	}
	if dedup.unavailable || fullHash == "" {
		return branchdam.ContentCheckResult{}, false
	}
	call, ok := e.contentCheckCall(ctx, config.DefaultPreflightTimeoutSecs)
	if !ok {
		return branchdam.ContentCheckResult{}, false
	}

	fullForm := branchdam.ContentCheckQuery{FastHash: fastHash, FullHash: fullHash}
	var err error
	if dedup.fullOnly {
		res, err = call(fullForm)
	} else {
		res, err = call(branchdam.ContentCheckQuery{FastHash: fastHash, SizeBytes: size, FullHash: fullHash})
		if err != nil && isBadRequest(err) {
			dedup.fullOnly = true
			slog.Info("ingest: server rejected the content lookup with 400 (older server) -- using the full-hash lookup for this pass", "source", srcPath, "err", err)
			res, err = call(fullForm)
		}
	}
	if err != nil {
		dedup.noteError(err)
		slog.Warn("ingest: could not ask the server whether an already-present copy is registered -- registering it", "source", srcPath, "err", err)
		return branchdam.ContentCheckResult{}, false
	}
	if res.Found && isLiveLifecycleState(res.LifecycleState) {
		return res, true
	}
	return branchdam.ContentCheckResult{}, false
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

// dedupState is one ingest pass's memory of how the server answered the
// content dedup check, so a pass does not keep asking a question the server
// cannot answer. It lives for one IngestCard/IngestCardOffline call; the
// next pass (the next card) starts fresh, since the server may have been
// upgraded or come back in between.
type dedupState struct {
	// unavailable: the endpoint is missing (404/501), the server is
	// unreachable (transport error, timeout), refuses the pass's API key
	// (401/403) or asks the agent to back off (429). The remaining files of the
	// pass skip the check entirely (fail-open).
	unavailable bool
	// fullOnly: the server rejected the fast-hash pre-screen with 400, so
	// it predates the pre-screen. The remaining files of the pass go
	// straight to the full-hash lookup, which such a server does support;
	// dedup keeps working.
	fullOnly bool
}

// noteError records what a failed check-content call says about the
// server: a transport error, 404/501 (no such endpoint), 401/403 (the
// pass's API key is refused -- it is loaded once per pass, so every later
// file would be refused too) or 429 (back off) latches unavailable; any
// other status (400, other 5xx, ...) is about this request only and
// latches nothing. Callers log the failure before calling this, so the
// failure that latches is the one logged at Warn.
func (d *dedupState) noteError(err error) {
	var he *branchdam.HTTPError
	if !errors.As(err, &he) {
		d.unavailable = true
		return
	}
	switch he.StatusCode {
	case http.StatusNotFound, http.StatusNotImplemented,
		http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		d.unavailable = true
	}
}

// isBadRequest reports whether err is an HTTP 400 from the server.
func isBadRequest(err error) bool {
	var he *branchdam.HTTPError
	return errors.As(err, &he) && he.StatusCode == http.StatusBadRequest
}

// removeDuplicateFn is os.Remove, indirected so tests can make the
// removal of a duplicate's copy fail.
var removeDuplicateFn = os.Remove

// removeDuplicateCopy removes a copy this ingest pass itself just wrote
// (DualWrite's O_EXCL create or the offline temp name) after the server
// confirmed the content is already in the library. A failed removal is
// not fatal -- the content is safe in the library either way, and the
// leftover is a redundant copy, not a lost one -- but it is returned so the
// caller can report the leftover.
func removeDuplicateCopy(srcPath, p string) error {
	if err := removeDuplicateFn(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		slog.Warn("ingest: failed to remove copy of a duplicate file", "source", srcPath, "path", p, "err", err)
		return err
	}
	// Make the removal durable, as DualWrite does for the create: a crash
	// right after must not bring back an unregistered copy.
	if err := syncParentDirFn(filepath.Dir(p)); err != nil {
		slog.Warn("ingest: fsync after removing a duplicate copy failed", "path", p, "err", err)
	}
	return nil
}

// contentCheckCall returns a function that makes one check-content
// request bounded by the per-request pre-flight timeout
// (Ingest.PreflightTimeoutSecs, or defaultTimeoutSecs when that is 0), or
// ok=false when the check is disabled (PreflightTimeoutSecs < 0) or the
// client cannot make it.
func (e *Engine) contentCheckCall(ctx context.Context, defaultTimeoutSecs int) (call func(branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error), ok bool) {
	if e.Ingest.PreflightTimeoutSecs < 0 || e.Client == nil {
		return nil, false
	}
	checker, ok := e.Client.(contentChecker)
	if !ok {
		return nil, false
	}
	timeout := time.Duration(defaultTimeoutSecs) * time.Second
	if e.Ingest.PreflightTimeoutSecs > 0 {
		timeout = time.Duration(e.Ingest.PreflightTimeoutSecs) * time.Second
	}
	return func(q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		cCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return checker.CheckContent(cCtx, q)
	}, true
}

// checkContentDedup is the best-effort content dedup check against
// GET /api/v1/agent/check-content. It runs after the copy, on the hashes
// the copy computed in flight (wr.FastHash, wr.FullHash), so it never reads
// the card itself (invariant 1).
//
//  1. Fast pre-screen: fastHash + sizeBytes. possibleMatch=false (the usual
//     answer for new media) ends the check; the full hash never leaves the
//     agent.
//  2. On possibleMatch, confirm with fullHash. A found node in a live
//     state (ACTIVE or HIDDEN) is a duplicate: skip=true with the existing
//     node's UUID and a human-readable reason.
//
// A server predating the pre-screen answers step 1 with 400; that is not
// "dedup unavailable": the check goes straight to step 2, and dedup.fullOnly
// makes the rest of the pass do the same. Every other failure fails open
// (skip=false) and may latch dedup.unavailable (see noteError).
//
// ownContainerPath is the container path this file's own archive copy
// will be registered under (empty when there is none yet): a match on that
// exact path is the copy just written, not an earlier one, and is never
// treated as a duplicate.
func (e *Engine) checkContentDedup(ctx context.Context, srcPath string, wr WriteResult, ownContainerPath string, defaultTimeoutSecs int, dedup *dedupState) (skip bool, existingNodeUUID, skipReason string) {
	if dedup == nil {
		dedup = &dedupState{}
	}
	if dedup.unavailable || wr.FullHash == "" {
		return false, "", ""
	}
	call, ok := e.contentCheckCall(ctx, defaultTimeoutSecs)
	if !ok {
		return false, "", ""
	}
	// Failures below are logged at Warn unconditionally. dedup.unavailable
	// is never set by the time one is logged: the guard at the top of this
	// function returns before any request once it is, and noteError (which
	// latches it) runs only after the slog.Warn at both call sites. So the
	// failure that latches is the one logged at Warn and the files after it
	// never get this far. Keep that order -- guard first, noteError after the
	// log -- rather than branching on dedup.unavailable here.
	if !dedup.fullOnly {
		res, err := call(branchdam.ContentCheckQuery{FastHash: wr.FastHash, SizeBytes: wr.SizeBytes})
		switch {
		case err != nil && isBadRequest(err):
			dedup.fullOnly = true
			slog.Info("ingest: server rejected the fast-hash pre-screen (older server) -- using the full-hash lookup for this pass", "source", srcPath, "err", err)
		case err != nil:
			slog.Warn("ingest: content check pre-flight failed (fail-open)", "source", srcPath, "err", err)
			dedup.noteError(err)
			return false, "", ""
		case !res.PossibleMatch && !res.Found:
			return false, "", ""
		}
	}

	res, err := call(branchdam.ContentCheckQuery{FastHash: wr.FastHash, FullHash: wr.FullHash})
	if err != nil {
		slog.Warn("ingest: content check pre-flight confirmation failed (fail-open)", "source", srcPath, "err", err)
		dedup.noteError(err)
		return false, "", ""
	}
	if !res.Found || !isLiveLifecycleState(res.LifecycleState) {
		return false, "", ""
	}
	if ownContainerPath != "" && res.FilePath == ownContainerPath {
		// The server already indexed the copy this run just wrote (e.g. a
		// watcher on the archive): that is this file, not a duplicate.
		return false, "", ""
	}
	// Known window: check-content names one node per hash. If a server
	// watcher indexed this run's archive copy in the moments between
	// DualWrite and this answer, but the answer names an older node, the
	// caller still removes the copy, and the watcher then marks the node
	// it just created MISSING (rows are never deleted server-side). No
	// content is lost -- the older node holds it -- and no endpoint lists
	// every node with a hash, so a re-query could not close the window.
	return true, res.NodeUUID, fmt.Sprintf("duplicate: already in library as node %s at %s", res.NodeUUID, res.FilePath)
}
