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
//     exiftool preview extraction: try "-b -PreviewImage", then
//     "-b -JpgFromRaw", then "-b -ThumbnailImage" (probe.go:239-249), each
//     with the "--" path-injection separator, taking the first whose
//     stdout both exists and itself decodes via image.Decode
//     (probe.go:582-593). Hash that.
//  3. If exiftool is unavailable, or none of the three tags yield a
//     decodable image, return (nil, nil) -- not an error. That's the normal
//     case for a file with no embedded preview, not a failure.
package phash

import (
	"bytes"
	"context"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"

	"github.com/s3ntin3l8/branchdam-agent/internal/hashing"
)

// previewImageArgs/jpgFromRawArgs/thumbnailImageArgs are copied verbatim
// from branchDAM's internal/probe/probe.go:239-249, including the "--"
// separator that keeps an adversarial path from being parsed as an
// exiftool flag.
func previewImageArgs(path string) []string {
	return []string{"-b", "-PreviewImage", "--", path}
}

func jpgFromRawArgs(path string) []string {
	return []string{"-b", "-JpgFromRaw", "--", path}
}

func thumbnailImageArgs(path string) []string {
	return []string{"-b", "-ThumbnailImage", "--", path}
}

// previewArgFuncs is the fixed, ordered fallback chain -- order matters,
// see the package doc.
var previewArgFuncs = []func(string) []string{previewImageArgs, jpgFromRawArgs, thumbnailImageArgs}

// Extract computes a perceptual hash for the file at path, following the
// same decode-then-exiftool-fallback sequence as branchDAM's
// probe.(*Prober).ExtractPHash. exiftoolPath is the resolved path to the
// exiftool binary (empty string means "unavailable", mirroring
// Prober.HasExiftool()'s effect on ExtractPreviewJPEG). Returns (nil, nil)
// -- not an error -- when no hash could be produced; that is the expected
// outcome for a non-image file or a RAW with no usable embedded preview,
// not a failure.
func Extract(ctx context.Context, exiftoolPath, path string) (*int64, error) {
	if ph, err := decodeFileAndHash(path); err == nil {
		return ph, nil
	}

	preview, err := extractPreviewJPEG(ctx, exiftoolPath, path)
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
// previewArgFuncs order, skip a tag whose command fails or produces no
// stdout, skip a tag whose stdout doesn't itself decode, and return the
// first that clears both bars. Returns (nil, nil) when exiftoolPath is
// empty (tool unavailable) or every tag was exhausted with nothing usable --
// neither is an error.
func extractPreviewJPEG(ctx context.Context, exiftoolPath, path string) ([]byte, error) {
	if exiftoolPath == "" {
		return nil, nil
	}
	for _, argsFn := range previewArgFuncs {
		cmd := exec.CommandContext(ctx, exiftoolPath, argsFn(path)...)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil || stdout.Len() == 0 {
			continue
		}
		if _, _, err := image.Decode(bytes.NewReader(stdout.Bytes())); err != nil {
			continue
		}
		return stdout.Bytes(), nil
	}
	return nil, nil
}
