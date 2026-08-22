package ingest

import (
	"path/filepath"
	"strings"
)

// videoExts/imageExts are copied verbatim from branchDAM's
// internal/pipeline/result.go (commit c570690) -- the closed extension sets
// that gate FFProbe/pHash attempts during a server-side scan. Parity here
// matters for the same reason as everywhere else in this package: an agent
// that attempted pHash on a different file-type set than the server would
// produce a phash where the server has none (or vice versa), which the
// parity test would catch as a mismatch on files it was never meant to
// disagree on.
var videoExts = map[string]bool{
	"mp4": true, "mov": true, "m4v": true, "mkv": true, "avi": true,
	"webm": true, "wmv": true, "mts": true, "m2ts": true, "ts": true,
	"3gp": true, "flv": true, "mpg": true, "mpeg": true, "lrf": true,
}

var imageExts = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true,
	"tif": true, "tiff": true, "bmp": true, "heic": true, "heif": true,
	"arw": true, "cr2": true, "cr3": true, "nef": true, "dng": true,
	"raf": true, "rw2": true, "orf": true, "pef": true, "srw": true,
}

func isVideoExt(ext string) bool {
	return videoExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
}

func isImageExt(ext string) bool {
	return imageExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
}

// extNoDot mirrors branchDAM's own pipeline.scan.go convention for
// media_nodes.file_ext / NodeCreatedPayload.FileExt: lowercase, no leading
// dot.
func extNoDot(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}
