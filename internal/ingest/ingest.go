package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/naming"
	"github.com/s3ntin3l8/branchdam-agent/internal/phash"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// nodeCreator is the subset of *branchdam.Client's surface Engine needs, so
// tests can substitute a fake without a real HTTP server.
type nodeCreator interface {
	PostNodeCreated(ctx context.Context, agentID string, payload branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error)
}

// Engine drives one full ingest run over a card's contents: metadata
// extraction, the dual-copy verified write, DJI .srt GPS handling, and
// submission via the branchDAM agent-event client. Plain library code, no
// UI imports -- cmd/branchdam-agent's `ingest` subcommand and a later tray
// PR are both thin drivers over this type.
type Engine struct {
	Client   nodeCreator
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
}

// NewEngine builds an Engine with real clock/UUID/exiftool dependencies.
func NewEngine(client nodeCreator, agentID string, ingestCfg config.IngestConfig, mappings []config.PathMapping) *Engine {
	return &Engine{
		Client:      client,
		AgentID:     agentID,
		Ingest:      ingestCfg,
		Mappings:    mappings,
		Exiftool:    NewExiftool(),
		Now:         time.Now,
		NewNodeUUID: func() (string, error) { id, err := uuid.NewV7(); return id.String(), err },
	}
}

// FileResult is one card file's outcome.
type FileResult struct {
	SourcePath    string
	ArchivePath   string
	LocalPath     string
	IsSidecar     bool // .xmp/.srt -- copied to both destinations, no event submitted
	Skipped       bool
	SkipReason    string
	NodeUUID      string
	EventID       string
	Write         WriteResult
	ArchiveVerify VerifyResult
	LocalVerify   VerifyResult
	Exif          *ExifResult
	PHash         *int64
	GPSSource     string // "exif", "srt", or "" (no GPS)
	Err           error
}

// CardResult is one IngestCard call's full outcome.
type CardResult struct {
	Files []FileResult
}

// IngestCard walks cardRoot (a mounted card's root directory, or the
// fixture directory a test points --card at) and ingests every regular
// file found under it. Returns per-file results even when some files
// failed -- a single bad file must not abort the rest of the card, matching
// the same "log and continue" spirit as branchDAM's own scan pipeline.
func (e *Engine) IngestCard(ctx context.Context, cardRoot string) (CardResult, error) {
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
		return CardResult{}, fmt.Errorf("ingest: walk card root %s: %w", cardRoot, err)
	}

	var result CardResult
	for _, f := range files {
		fr := e.ingestFile(ctx, f)
		result.Files = append(result.Files, fr)
	}
	return result, nil
}

// ingestFile runs the full pipeline for one source file: metadata
// extraction (needed up front to fill the naming template), dual-copy
// write, verify, DJI .srt GPS, and (for non-sidecar files) submission.
func (e *Engine) ingestFile(ctx context.Context, srcPath string) FileResult {
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

	relPath := RenderPath(e.Ingest.PathTemplate, vars)
	archivePath := filepath.Join(e.Ingest.ArchiveRoot, relPath)
	localPath := filepath.Join(e.Ingest.LocalEditRoot, relPath)
	fr.ArchivePath = archivePath
	fr.LocalPath = localPath

	writeRes, err := DualWrite(srcPath, archivePath, localPath)
	if err != nil {
		fr.Err = fmt.Errorf("dual write: %w", err)
		return fr
	}
	fr.Write = writeRes

	// Preserve the source's original mtime on both destinations -- an
	// ingested master's mtime should reflect when it was captured/written
	// on the card, not the moment this agent happened to copy it.
	_ = os.Chtimes(archivePath, e.now(), srcInfo.ModTime())
	_ = os.Chtimes(localPath, e.now(), srcInfo.ModTime())

	archiveVerify, err := Verify(archivePath, writeRes.FullHash)
	if err != nil {
		fr.Err = fmt.Errorf("verify archive copy: %w", err)
		return fr
	}
	fr.ArchiveVerify = archiveVerify
	localVerify, err := Verify(localPath, writeRes.FullHash)
	if err != nil {
		fr.Err = fmt.Errorf("verify local copy: %w", err)
		return fr
	}
	fr.LocalVerify = localVerify

	if !archiveVerify.Verified || !localVerify.Verified {
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
