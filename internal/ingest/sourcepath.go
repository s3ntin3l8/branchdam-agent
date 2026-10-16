package ingest

import (
	"crypto/sha256"
	"encoding/hex"
)

// sourcePathHash is the lowercase SHA-256 hex of srcPath (the file's path
// on the card or source folder, exactly as walked) -- what
// EVENT_NODE_CREATED carries as sourcePathHash and what the streaming
// upload sends as X-Source-Path-Hash. The path itself never leaves the
// workstation; the server stores only this digest.
func sourcePathHash(srcPath string) string {
	sum := sha256.Sum256([]byte(srcPath))
	return hex.EncodeToString(sum[:])
}
