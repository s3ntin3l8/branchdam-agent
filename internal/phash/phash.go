// Package phash ports the *call sequence* of branchDAM's
// internal/probe.(*Prober).ExtractPHash (commit c570690, probe.go:656-706),
// not a reimplementation of exiftool or the DCT hash itself. The hash
// itself is exact by construction (internal/hashing.PerceptualHash wraps
// github.com/corona10/goimagehash, the identical public dependency
// branchDAM uses); what has to be ported carefully is the decode-then-
// fallback order, because getting that wrong silently changes which bytes
// get hashed for any file that isn't directly decodable (RAW formats).
//
// Sequence, matching probe.go exactly:
//  1. Try direct image.Decode on the file, with EXACTLY image/gif,
//     image/jpeg, image/png registered (probe.go:42-44) -- no more, no
//     fewer. On success, hash the decoded image.
//  2. On any failure (open error, decode error, or a hash error after a
//     successful decode -- probe.go's decodeFileAndHash folds all three
//     into a single err check its caller treats identically), fall back to
//     exiftool preview extraction: PreviewImage, JpgFromRaw, and
//     ThumbnailImage are all requested in a single exiftool invocation
//     (base64-encoded JSON values, via "-j -b"), then tried client-side in
//     that fixed order (probe.go:239-249), taking the first whose decoded
//     bytes both exist and themselves decode via image.Decode
//     (probe.go:582-593). Hash that.
//  3. If exiftool is unavailable, or none of the three tags yield a
//     decodable image, return (nil, nil) -- not an error. That's the normal
//     case for a file with no embedded preview, not a failure.
package phash

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/exiftool"
	"github.com/s3ntin3l8/branchdam-agent/internal/hashing"
)

// previewTags is the fixed, ordered fallback chain -- order matters, see
// the package doc -- and doubles as the exact set of tags requested in the
// single combined exiftool invocation previewArgs builds.
var previewTags = []string{"PreviewImage", "JpgFromRaw", "ThumbnailImage"}

// executor is the subset of *exiftool.Pool's surface extractPreviewJPEG
// needs, so tests can substitute a fake without a real exiftool
// subprocess. Extract itself takes a concrete *exiftool.Pool, not this
// interface -- converting a nil *exiftool.Pool to executor would produce a
// non-nil interface wrapping a nil pointer, so the nil check has to happen
// on the concrete type before extractPreviewJPEG is ever called.
type executor interface {
	Execute(ctx context.Context, args []string) ([]byte, error)
}

// previewArgs builds the single exiftool invocation that requests all of
// previewTags at once, base64-encoded in JSON output, from one pooled
// process instead of one process per tag. "--" is added only when path
// actually needs it (see exiftool.NeedsSeparator).
func previewArgs(path string) []string {
	args := append([]string{"-j", "-b", "-n"}, tagFlags()...)
	if exiftool.NeedsSeparator(path) {
		args = append(args, "--")
	}
	return append(args, path)
}

func tagFlags() []string {
	flags := make([]string, len(previewTags))
	for i, tag := range previewTags {
		flags[i] = "-" + tag
	}
	return flags
}

// Extract computes a perceptual hash for the file at path, following the
// same decode-then-exiftool-fallback sequence as branchDAM's
// probe.(*Prober).ExtractPHash. pool is the caller's pooled exiftool
// subprocess manager (nil means "unavailable", mirroring
// Prober.HasExiftool()'s effect on ExtractPreviewJPEG). Returns (nil, nil)
// -- not an error -- when no hash could be produced; that is the expected
// outcome for a non-image file or a RAW with no usable embedded preview,
// not a failure.
func Extract(ctx context.Context, pool *exiftool.Pool, path string) (*int64, error) {
	if ph, err := decodeFileAndHash(path); err == nil {
		return ph, nil
	}
	if pool == nil {
		return nil, nil
	}

	preview, err := extractPreviewJPEG(ctx, pool, path)
	if err != nil || len(preview) == 0 {
		return nil, nil
	}
	img, _, err := image.Decode(bytes.NewReader(preview))
	if err != nil {
		return nil, nil
	}
	hash, err := hashing.PerceptualHash(img)
	if err != nil {
		return nil, nil
	}
	return &hash, nil
}

// decodeFileAndHash mirrors probe.go's function of the same name exactly:
// open, decode via the package-registered format set, hash. Any of the
// three failing (open, decode, or hash) is folded into a single err return,
// which Extract's caller treats as "fall through to the exiftool preview
// chain" regardless of which of the three actually failed.
func decodeFileAndHash(path string) (*int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	hash, err := hashing.PerceptualHash(img)
	if err != nil {
		return nil, err
	}
	return &hash, nil
}

// extractPreviewJPEG mirrors probe.go's ExtractPreviewJPEG: try each tag in
// previewTags order, skip a tag that's absent or undecodable, and return
// the first that clears both bars. All three tags come from a single
// pooled exiftool invocation (previewArgs) rather than one invocation per
// tag. Returns (nil, nil) when every tag was exhausted with nothing
// usable, or the request itself failed -- neither is an error.
func extractPreviewJPEG(ctx context.Context, pool executor, path string) ([]byte, error) {
	stdout, err := pool.Execute(ctx, previewArgs(path))
	if err != nil {
		return nil, nil
	}

	var rows []map[string]any
	if err := json.Unmarshal(stdout, &rows); err != nil || len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]

	for _, tag := range previewTags {
		data, ok := decodeExiftoolBinary(row[tag])
		if !ok {
			continue
		}
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			continue
		}
		return data, nil
	}
	return nil, nil
}

// decodeExiftoolBinary decodes one exiftool JSON binary tag value: with
// "-b -j", exiftool represents binary tag data as a string of the form
// "base64:<data>". A missing tag (v is nil, the key wasn't in the row) or
// any other shape is reported as !ok, not an error -- both are the normal
// "this file has no such preview" case.
func decodeExiftoolBinary(v any) ([]byte, bool) {
	s, ok := v.(string)
	if !ok {
		return nil, false
	}
	encoded, ok := strings.CutPrefix(s, "base64:")
	if !ok {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	return data, true
}
