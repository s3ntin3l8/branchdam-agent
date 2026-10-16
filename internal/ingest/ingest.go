package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/zeebo/blake3"

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
	var dedupUnavailable bool
	var result CardResult
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
		result.Files = append(result.Files, e.ingestFile(ctx, path, stemSuffix, &dedupUnavailable))
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return result, fmt.Errorf("ingest: card %s: stopped before the remaining files: %w", cardRoot, err)
		}
		return CardResult{}, fmt.Errorf("ingest: walk card root %s: %w", cardRoot, err)
	}
	return result, nil
}

// ingestFile runs the full pipeline for one source file: metadata
// extraction (needed up front to fill the naming template), dual-copy
// write, verify, DJI .srt GPS, and (for non-sidecar files) registration.
//
// Ordering matters for what a failure leaves behind. Everything that can
// fail without touching the destinations -- naming, the archive path's
// container mapping, the pre-flight dedup -- runs before DualWrite. A
// failed verify (or an unbuffered verify the config requires but didn't
// get) removes both copies this call just created. Once both copies are
// verified the file is registered by registerNode, which records the
// intent in queue.db before the POST. And a later run that finds the
// copies already in place does not take that as proof of registration
// (confirmRegistered).
func (e *Engine) ingestFile(ctx context.Context, srcPath string, stemSuffix map[string]string, dedupUnavailable *bool) FileResult {
	// UploadStream mode sends bytes directly to the server upload endpoint;
	// server-side dedup is the backstop for that path.
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
		return e.confirmRegistered(ctx, fr, srcInfo, exif, dedupUnavailable)
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

	// Pre-flight BLAKE3 content dedup check before DualWrite.
	if !fr.IsSidecar {
		if skip, existingUUID, skipReason := e.checkContentDedup(ctx, srcPath, srcInfo.Size(), config.DefaultPreflightTimeoutSecs, dedupUnavailable); skip {
			fr.Skipped = true
			fr.SkipReason = skipReason
			fr.ExistingNodeUUID = existingUUID
			return fr
		}
	}

	writeRes, err := DualWrite(srcPath, archivePath, localPath, e.progressOpts(ctx, localPath, ProgressPhaseCopying, srcInfo.Size())...)
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
//     not the card) and ask the server's check-content. A live node with
//     this content: skip.
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
func (e *Engine) confirmRegistered(ctx context.Context, fr FileResult, srcInfo os.FileInfo, exif *ExifResult, dedupUnavailable *bool) FileResult {
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

	if found, nodeUUID := e.serverHasLiveContent(ctx, fastHash, fullHash, dedupUnavailable); found {
		fr.Skipped = true
		fr.ExistingNodeUUID = nodeUUID
		fr.SkipReason = fmt.Sprintf("%s; server already has it as node %s", alreadyIngestedReason, nodeUUID)
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

	containerPath, err := ToContainerPath(e.Mappings, archivePath)
	if err != nil {
		fr.Err = err
		return fr
	}
	fr.Write = WriteResult{ArchivePath: archivePath, LocalPath: localPath, SizeBytes: size, FullHash: fullHash, FastHash: fastHash}
	slog.Info("ingest: copies already at destination are not known to be registered -- registering them", "source", fr.SourcePath, "archive", archivePath)
	e.registerNode(ctx, &fr, srcInfo, exif, containerPath, fr.Write)
	return fr
}

// serverHasLiveContent asks GET /api/v1/agent/check-content whether a
// live node already has this content. found=false covers "not there" and
// "couldn't ask" alike (no checker, pre-flight disabled, the endpoint
// latched unavailable earlier in this pass, or an error) -- the caller
// then registers, which the server dedups by full_hash if it does have it.
func (e *Engine) serverHasLiveContent(ctx context.Context, fastHash, fullHash string, dedupUnavailable *bool) (found bool, nodeUUID string) {
	if (dedupUnavailable != nil && *dedupUnavailable) || e.Ingest.PreflightTimeoutSecs < 0 || e.Client == nil {
		return false, ""
	}
	checker, ok := e.Client.(contentChecker)
	if !ok {
		return false, ""
	}
	timeout := time.Duration(config.DefaultPreflightTimeoutSecs) * time.Second
	if e.Ingest.PreflightTimeoutSecs > 0 {
		timeout = time.Duration(e.Ingest.PreflightTimeoutSecs) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	res, err := checker.CheckContent(cctx, fastHash, fullHash)
	cancel()
	if err != nil {
		slog.Warn("ingest: could not ask the server whether an already-present copy is registered -- registering it", "err", err)
		return false, ""
	}
	if res.Found && isLiveLifecycleState(res.LifecycleState) {
		return true, res.NodeUUID
	}
	return false, ""
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

	if e.Ingest.PauseUploadOnMetered {
		if metered, mErr := e.IsMetered(); metered || mErr != nil {
			if mErr != nil {
				slog.Debug("metered probe failed, treating as metered (fail-closed)", "err", mErr)
			}
			slog.Info("upload skipped on metered connection -- re-insert card to retry", "path", srcPath)

			vars := buildTemplateVars(exif, srcPath, srcInfo.ModTime())

			relPath := RenderPath(e.Ingest.PathTemplate, vars)
			localPath := filepath.Join(e.Ingest.LocalEditRoot, relPath)
			fr.LocalPath = localPath

			writeRes, err := WriteLocal(srcPath, localPath, e.progressOpts(ctx, localPath, ProgressPhaseCopying, srcInfo.Size())...)
			if err != nil {
				fr.Err = fmt.Errorf("write local copy: %w", err)
				return fr
			}
			fr.Write = writeRes
			preserveMtimeAt(srcPath, localPath, srcInfo.ModTime())

			localVerify, err := Verify(localPath, writeRes.FullHash, e.progressOpts(ctx, localPath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
			if err != nil {
				_ = os.Remove(localPath)
				fr.Err = fmt.Errorf("verify local copy: %w", err)
				return fr
			}
			fr.LocalVerify = localVerify

			if !localVerify.Verified {
				_ = os.Remove(localPath)
				fr.Err = fmt.Errorf("ingest: verification failed for %s -- safe-eject withheld", srcPath)
				return fr
			}

			if e.Ingest.RequireUnbuffered && localVerify.Method == VerifyMethodBufferedFloor {
				fr.Err = fmt.Errorf("ingest: unbuffered verify required by config, but verify fell back to buffered floor (local=%s) -- safe-eject withheld", localVerify.Method)
				return fr
			}

			if isImageExt(ext) && e.Exiftool != nil {
				if ph, err := phash.Extract(ctx, e.Exiftool.Pool(), localPath); err == nil {
					fr.PHash = ph
				}
			}

			fr.Skipped = true
			fr.Deferred = true
			fr.SkipReason = "upload skipped on metered connection -- re-insert card to retry"
			return fr
		}
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

	srcPathHash := sha256.Sum256([]byte(srcPath))
	uploadOpts := branchdam.UploadOptions{
		Filename:         filepath.Base(srcPath),
		CameraModel:      cameraModel,
		CaptureTimestamp: captureTimestamp,
		SourcePathHash:   hex.EncodeToString(srcPathHash[:]),
	}

	var body io.Reader = srcFile
	if o := applyWriteOptions(e.progressOpts(ctx, srcPath, ProgressPhaseCopying, srcInfo.Size())); o.onBytes != nil {
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

	writeRes, err := WriteLocal(srcPath, localPath, e.progressOpts(ctx, localPath, ProgressPhaseCopying, srcInfo.Size())...)
	if err != nil {
		fr.Err = fmt.Errorf("write local copy: %w", err)
		return fr
	}
	fr.Write = writeRes
	// Soft contract (issue #103): the archive landed server-side via
	// Upload, so the only Chtimes call on this path is the local edit
	// copy. Failure is logged, not fatal -- see preserveMtimeAt.
	preserveMtimeAt(srcPath, localPath, srcInfo.ModTime())

	localVerify, err := Verify(localPath, upResp.Blake3Hash, e.progressOpts(ctx, localPath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
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
		if ph, err := phash.Extract(ctx, e.Exiftool.Pool(), localPath); err == nil {
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

// checkContentDedup performs the best-effort pre-flight BLAKE3 content dedup check.
// It queries GET /api/v1/agent/check-content with fastHash (xxHash64 sampled via fastHashFile
// using canonical 2MiB regions matching the server's fast_hash column), and on a hit
// confirms with fullHash (BLAKE3-256). If confirmed duplicate with a live lifecycle state,
// returns skip=true along with the existing node UUID and human-readable skip reason.
func (e *Engine) checkContentDedup(ctx context.Context, srcPath string, srcSize int64, defaultTimeoutSecs int, dedupUnavailable *bool) (skip bool, existingNodeUUID, skipReason string) {
	if (dedupUnavailable != nil && *dedupUnavailable) || e.Ingest.PreflightTimeoutSecs < 0 || e.Client == nil {
		return false, "", ""
	}
	checker, ok := e.Client.(contentChecker)
	if !ok {
		return false, "", ""
	}
	timeout := time.Duration(defaultTimeoutSecs) * time.Second
	if e.Ingest.PreflightTimeoutSecs > 0 {
		timeout = time.Duration(e.Ingest.PreflightTimeoutSecs) * time.Second
	}

	fastHash, err := fastHashFile(srcPath, srcSize)
	if err != nil {
		slog.Warn("ingest: pre-flight fast hash failed", "source", srcPath, "err", err)
		return false, "", ""
	}

	pCtx, pCancel := context.WithTimeout(ctx, timeout)
	res, err := checker.CheckContent(pCtx, fastHash, "")
	pCancel()
	if err != nil {
		if dedupUnavailable != nil && *dedupUnavailable {
			slog.Debug("ingest: content check pre-flight failed (fail-open)", "source", srcPath, "err", err)
		} else {
			slog.Warn("ingest: content check pre-flight failed (fail-open)", "source", srcPath, "err", err)
		}
		if dedupUnavailable != nil {
			var he *branchdam.HTTPError
			if !errors.As(err, &he) || (he.StatusCode == http.StatusNotFound || he.StatusCode == http.StatusNotImplemented || he.StatusCode == http.StatusBadRequest) {
				*dedupUnavailable = true
			}
		}
		return false, "", ""
	}

	if !res.Found {
		return false, "", ""
	}

	fullHash, err := blake3File(srcPath)
	if err != nil {
		slog.Warn("ingest: pre-flight full hash failed", "source", srcPath, "err", err)
		return false, "", ""
	}

	pCtx2, pCancel2 := context.WithTimeout(ctx, timeout)
	res2, err := checker.CheckContent(pCtx2, fastHash, fullHash)
	pCancel2()
	if err != nil {
		if dedupUnavailable != nil && *dedupUnavailable {
			slog.Debug("ingest: content check pre-flight confirmation failed (fail-open)", "source", srcPath, "err", err)
		} else {
			slog.Warn("ingest: content check pre-flight confirmation failed (fail-open)", "source", srcPath, "err", err)
		}
		if dedupUnavailable != nil {
			var he *branchdam.HTTPError
			if !errors.As(err, &he) || (he.StatusCode == http.StatusNotFound || he.StatusCode == http.StatusNotImplemented || he.StatusCode == http.StatusBadRequest) {
				*dedupUnavailable = true
			}
		}
		return false, "", ""
	}

	if res2.Found && isLiveLifecycleState(res2.LifecycleState) {
		return true, res2.NodeUUID, fmt.Sprintf("duplicate: already in library as node %s at %s", res2.NodeUUID, res2.FilePath)
	}
	return false, "", ""
}
