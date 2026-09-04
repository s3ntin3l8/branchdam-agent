package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/exiftool"
)

// ErrExiftoolUnavailable mirrors branchDAM's probe.ErrToolUnavailable --
// same name pattern, separate sentinel since this package cannot import
// branchDAM's internal/probe (different module).
var ErrExiftoolUnavailable = errors.New("ingest: exiftool is not installed")

// ExifResult holds exactly the promoted-column fields the conformance
// contract requires (see the plan doc's "conformance contract" table) plus
// GPS -- deliberately not the full exifMetadata/flattenToStrings overflow a
// server-side scan additionally writes to node_metadata, since
// NodeCreatedPayload has no generic metadata map to carry that in (a stated,
// not-closable-client-side limitation -- see issue #2's linked plan section
// "Promoted-column parity... not full parity").
type ExifResult struct {
	CameraModel        string // EXIF:Model, NOT Make+Model
	CameraSerial       string // EXIF:SerialNumber
	LensModel          string // EXIF:LensModel
	OriginalDocumentID string // XMP:OriginalDocumentID
	DocumentID         string // XMP:DocumentID
	DerivedFromID      string // XMP:DerivedFrom
	CapturedAt         *time.Time
	// GPSLatitude/GPSLongitude are signed decimal degrees (Composite group),
	// never the unsigned EXIF:GPSLatitude/GPSLongitude magnitude.
	GPSLatitude  *float64
	GPSLongitude *float64
}

// exiftoolArgs is the same fixed allowlist flags as branchDAM's
// probe.exiftoolArgs -- JSON output, grouped tag names, numeric values --
// plus a "--" path-injection guard, added only when path actually needs it
// (see exiftool.NeedsSeparator: an unconditional "--" would be safe for a
// one-shot fork but permanently wedges a pooled -stay_open process).
func exiftoolArgs(path string) []string {
	if exiftool.NeedsSeparator(path) {
		return []string{"-j", "-n", "-G", "--", path}
	}
	return []string{"-j", "-n", "-G", path}
}

const sidecarExt = ".xmp"

// sidecarPath mirrors branchDAM's probe.sidecarPath exactly: same
// directory, same stem (path's own last extension stripped, case
// preserved), ".xmp" extension. Deliberately not run through
// internal/naming.Stem -- see probe.go's doc comment, ported verbatim in
// spirit here: this is an on-disk sidecar lookup, not a graph-identity
// comparison.
func sidecarPath(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + sidecarExt
}

var sidecarBookkeepingPrefixes = []string{"File:", "ExifTool:"}

// mergeSidecarRow overlays sidecar's tags onto row, sidecar-wins, skipping
// per-file bookkeeping groups and "SourceFile" -- byte-for-byte port of
// branchDAM's probe.mergeSidecarRow.
func mergeSidecarRow(row, sidecar map[string]any) {
	for k, v := range sidecar {
		if k == "SourceFile" {
			continue
		}
		bookkeeping := false
		for _, prefix := range sidecarBookkeepingPrefixes {
			if strings.HasPrefix(k, prefix) {
				bookkeeping = true
				break
			}
		}
		if bookkeeping {
			continue
		}
		row[k] = v
	}
}

// exifTimeLayout/exifTimeLayoutNoOffset/capturedAt are ported verbatim from
// probe.go, including parsing an offset-less DateTimeOriginal as UTC (Go's
// default for an unzoned layout) -- getting this "wrong" relative to local
// time is required for parity with the server, which does the same thing.
const (
	exifTimeLayout         = "2006:01:02 15:04:05-07:00"
	exifTimeLayoutNoOffset = "2006:01:02 15:04:05"
)

func capturedAt(row map[string]any) *time.Time {
	if s := valString(row, "Composite:SubSecDateTimeOriginal"); s != "" {
		if t, err := time.Parse(exifTimeLayout, s); err == nil {
			return &t
		}
	}

	dto := valString(row, "EXIF:DateTimeOriginal")
	if dto == "" {
		return nil
	}
	if offset := valString(row, "EXIF:OffsetTimeOriginal"); offset != "" {
		if t, err := time.Parse(exifTimeLayout, dto+offset); err == nil {
			return &t
		}
	}
	if t, err := time.Parse(exifTimeLayoutNoOffset, dto); err == nil {
		return &t
	}
	return nil
}

func valString(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func valFloatPtr(row map[string]any, key string) *float64 {
	v, ok := row[key]
	if !ok {
		return nil
	}
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	return &f
}

// Exiftool wraps a resolved exiftool binary path plus a pooled exiftool
// subprocess, mirroring branchDAM's probe.Prober but scoped to only what
// this agent needs (read + sidecar merge; no WriteTags, no ffprobe/ffmpeg
// -- those stay server-side).
type Exiftool struct {
	path string
	pool *exiftool.Pool
}

// NewExiftool resolves exiftool via exec.LookPath. A missing binary is not
// an error here -- HasExiftool tells callers to skip metadata extraction
// and fall back to fast_hash-only indexing, matching probe.go's own
// graceful-skip contract.
func NewExiftool() *Exiftool {
	path, err := exec.LookPath("exiftool")
	if err != nil {
		return &Exiftool{}
	}
	return newExiftoolAtResolvedPath(path)
}

// NewExiftoolAt builds an Exiftool bound to configuredPath (e.g.
// config.IngestConfig.ExiftoolPath). A bare command name is resolved
// through PATH via exec.LookPath, same as NewExiftool; an absolute path is
// used as-is if it exists and is executable. An empty configuredPath falls
// back to NewExiftool's own PATH discovery.
func NewExiftoolAt(configuredPath string) *Exiftool {
	if configuredPath == "" {
		return NewExiftool()
	}
	path, err := exec.LookPath(configuredPath)
	if err != nil {
		return &Exiftool{}
	}
	return newExiftoolAtResolvedPath(path)
}

func newExiftoolAtResolvedPath(path string) *Exiftool {
	return &Exiftool{path: path, pool: exiftool.NewPool(path)}
}

// HasExiftool reports whether exiftool was found at construction time.
func (e *Exiftool) HasExiftool() bool { return e.path != "" }

// Path returns the resolved exiftool binary path, or "" if unavailable.
func (e *Exiftool) Path() string { return e.path }

// Pool returns the pooled exiftool subprocess manager backing this
// Exiftool, or nil if none was constructed (e.g. exiftool unavailable, or
// this Exiftool was built as a bare struct literal rather than through
// NewExiftool/NewExiftoolAt -- a pattern this package's own tests use).
// Used by internal/phash.Extract, which takes the pool directly rather
// than depending on this type.
func (e *Exiftool) Pool() *exiftool.Pool { return e.pool }

// Close terminates every pooled exiftool subprocess. Safe to call on an
// Exiftool with no pool (e.g. exiftool unavailable).
func (e *Exiftool) Close() {
	if e.pool != nil {
		e.pool.Close()
	}
}

// execute runs exiftool with args through the pool when one is set,
// falling back to a plain one-shot fork otherwise -- the latter keeps a
// bare &Exiftool{path: ...} literal (as this package's own tests build)
// working without requiring every call site to go through a constructor.
func (e *Exiftool) execute(ctx context.Context, args []string) ([]byte, error) {
	if e.pool != nil {
		return e.pool.Execute(ctx, args)
	}
	cmd := exec.CommandContext(ctx, e.path, args...) //nolint:gosec // path is a resolved exiftool binary, args are caller-built
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("exiftool fork %v: %w (stderr: %s)", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Exif runs exiftool against path, merges a sibling .xmp sidecar's tags
// (sidecar-wins) when one exists, and extracts exactly the promoted-column
// fields. Returns ErrExiftoolUnavailable if exiftool wasn't found.
func (e *Exiftool) Exif(ctx context.Context, path string) (*ExifResult, error) {
	if !e.HasExiftool() {
		return nil, ErrExiftoolUnavailable
	}

	stdout, err := e.execute(ctx, exiftoolArgs(path))
	if err != nil {
		return nil, fmt.Errorf("ingest: exiftool %s: %w", path, err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(stdout, &rows); err != nil {
		return nil, fmt.Errorf("ingest: parse exiftool output for %s: %w", path, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("ingest: exiftool returned no result for %s", path)
	}
	row := rows[0]

	if sidecar, ok := e.readSidecarRow(ctx, path); ok {
		mergeSidecarRow(row, sidecar)
	}

	result := &ExifResult{
		CameraModel:        valString(row, "EXIF:Model"),
		CameraSerial:       valString(row, "EXIF:SerialNumber"),
		LensModel:          valString(row, "EXIF:LensModel"),
		OriginalDocumentID: valString(row, "XMP:OriginalDocumentID"),
		DocumentID:         valString(row, "XMP:DocumentID"),
		DerivedFromID:      valString(row, "XMP:DerivedFrom"),
		GPSLatitude:        valFloatPtr(row, "Composite:GPSLatitude"),
		GPSLongitude:       valFloatPtr(row, "Composite:GPSLongitude"),
	}
	result.CapturedAt = capturedAt(row)
	return result, nil
}

// readSidecarRow mirrors branchDAM's probe.readSidecarRow: a fixed-point
// guard against path itself already being .xmp, os.Lstat (never follows a
// symlink) + Mode().IsRegular() (rejects a FIFO that would otherwise hang
// exiftool), and a second exiftool invocation through the exact same
// never-writes argv.
func (e *Exiftool) readSidecarRow(ctx context.Context, path string) (map[string]any, bool) {
	if strings.EqualFold(filepath.Ext(path), sidecarExt) {
		return nil, false
	}

	sidecar := sidecarPath(path)
	info, err := os.Lstat(sidecar)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}

	stdout, err := e.execute(ctx, exiftoolArgs(sidecar))
	if err != nil {
		return nil, false
	}

	var rows []map[string]any
	if err := json.Unmarshal(stdout, &rows); err != nil || len(rows) == 0 {
		return nil, false
	}
	return rows[0], true
}
