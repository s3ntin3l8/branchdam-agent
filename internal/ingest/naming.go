// Package ingest is the SD-card ingest core (issue #2, M1): card detection,
// the one-read/two-write dual-copy writer, cache-defeating verified writes,
// metadata extraction matching branchDAM's own promoted-column set, DJI
// .srt GPS handling, and submission via internal/branchdam's client. Plain
// library code with no UI imports -- cmd/branchdam-agent's `ingest`
// subcommand is the only driver in this PR; a later tray PR is a second,
// thin driver over the same package (see the plan doc's M1 section).
package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/hashing"
)

// DefaultPathTemplate matches the plan doc and issue #2's stated default.
const DefaultPathTemplate = "{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}"

// TemplateVars are the placeholder values RenderPath substitutes.
type TemplateVars struct {
	CapturedAt   time.Time // falls back to the source file's mtime when no capturedAt was extracted
	CameraModel  string    // falls back to "unknown_camera" when empty (EXIF absent, or no exiftool)
	OriginalName string    // the source file's base name, unchanged
}

// splitBase splits a filename into its stem and extension (without dot).
// If the filename has no extension (or is a dotfile with no additional extension),
// stem is the full name and ext is empty.
func splitBase(name string) (stem, ext string) {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return name, ""
	}
	return name[:dot], name[dot+1:]
}

// SuffixedFilename returns a filename with an optional suffix inserted before the
// extension. If suffix is empty, it returns original unchanged.
func SuffixedFilename(original, suffix string) string {
	if suffix == "" {
		return original
	}
	dot := strings.LastIndex(original, ".")
	if dot <= 0 {
		return original + suffix
	}
	return original[:dot] + suffix + original[dot:]
}

// sanitizeSegment replaces path separators and other characters that would
// either escape the destination root or produce an invalid path component
// on Windows (the plan's stated second target platform) with "_", then runs
// filepath.Clean on the result and rejects any segment that would resolve
// to ".", "..", or that still contains a literal ".." substring by
// returning a literal "_" instead. The Clean + reject pass is what
// enforces RenderPath's "never contains .." invariant (issue #99) against
// CameraModel/OriginalName values like "../../etc" that the
// character-only replacer would otherwise pass through. Templates are
// config-supplied, not attacker-controlled, but CameraModel and
// OriginalName both originate from file content/names on a removable card,
// which this agent does not otherwise trust.
func sanitizeSegment(s string) string {
	if s == "" {
		return s
	}
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	)
	cleaned := filepath.Clean(replacer.Replace(s))
	// filepath.Clean only resolves ".." in path context (with separators).
	// After the slash-replacement above, any ".." that remains is a
	// literal substring inside an otherwise-safe segment, so reject it
	// directly to close issue #99 -- e.g. "../../etc" becomes
	// ".._.._etc", which still contains "..".
	if cleaned == "." || cleaned == ".." || strings.Contains(cleaned, "..") {
		return "_"
	}
	return cleaned
}

// RenderPath expands tpl against vars, returning a slash-separated relative
// path (never absolute, never containing "..") suitable for joining under
// either ArchiveRoot or LocalEditRoot. Both destinations call this with
// the identical vars, which is what makes the local copy mirror the
// archive subtree by construction (issue #2's stated design). The
// "never containing .." guarantee is enforced by sanitizeSegment, which
// runs filepath.Clean on each token and falls back to "_" when the
// cleaned segment would be ".", "..", or would still contain a literal
// ".." substring (issue #99).
func RenderPath(tpl string, vars TemplateVars) string {
	if tpl == "" {
		tpl = DefaultPathTemplate
	}
	cameraModel := vars.CameraModel
	if cameraModel == "" {
		cameraModel = "unknown_camera"
	}
	stem, ext := splitBase(vars.OriginalName)

	replacer := strings.NewReplacer(
		"{yyyy}", vars.CapturedAt.Format("2006"),
		"{mm}", vars.CapturedAt.Format("01"),
		"{dd}", vars.CapturedAt.Format("02"),
		"{camera_model}", sanitizeSegment(cameraModel),
		"{original_name}", sanitizeSegment(vars.OriginalName),
		"{stem}", sanitizeSegment(stem),
		"{ext}", sanitizeSegment(ext),
	)
	return replacer.Replace(tpl)
}

// DestinationResolution holds the resolved relative path, the suffix used,
// and whether the destination already has an identical copy of the file.
type DestinationResolution struct {
	RelPath         string
	Suffix          string
	AlreadyIngested bool
}

// fastHashFile opens p and calculates its FastHash with size.
func fastHashFile(p string, size int64) (string, error) {
	f, err := os.Open(p) //nolint:gosec // path is our destination or source
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return hashing.FastHash(f, size)
}

// filesMatch reports whether dstPath exists, has the same size, and has the same
// FastHash digest as srcPath.
func filesMatch(srcPath, dstPath string) bool {
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return false
	}
	dstInfo, err := os.Stat(dstPath)
	if err != nil {
		return false
	}
	if srcInfo.Size() != dstInfo.Size() {
		return false
	}
	srcHash, err := fastHashFile(srcPath, srcInfo.Size())
	if err != nil {
		return false
	}
	dstHash, err := fastHashFile(dstPath, dstInfo.Size())
	if err != nil {
		return false
	}
	return srcHash == dstHash
}

// checkRoots checks if relPath exists in all non-empty roots and matches srcPath.
func checkRoots(roots []string, relPath, srcPath string) (allExist bool, allMatch bool) {
	validRoots := 0
	for _, root := range roots {
		if root == "" {
			continue
		}
		validRoots++
		p := filepath.Join(root, relPath)
		if _, err := os.Stat(p); err != nil {
			return false, false
		}
		if !filesMatch(srcPath, p) {
			return true, false
		}
	}
	return validRoots > 0, true
}

// ResolveDestination determines the relative destination path for srcPath under
// the provided roots, resolving any naming collisions by auto-suffixing (_2, _3, ...)
// and detecting whether an identical copy has already been ingested.
func ResolveDestination(roots []string, tpl string, vars TemplateVars, srcPath string, knownSuffix string) DestinationResolution {
	if knownSuffix != "" {
		candidateVars := vars
		candidateVars.OriginalName = SuffixedFilename(vars.OriginalName, knownSuffix)
		rel := RenderPath(tpl, candidateVars)
		allExist, allMatch := checkRoots(roots, rel, srcPath)
		if allExist && allMatch {
			return DestinationResolution{
				RelPath:         rel,
				Suffix:          knownSuffix,
				AlreadyIngested: true,
			}
		}
		anyExists := false
		for _, root := range roots {
			if root == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
				anyExists = true
				break
			}
		}
		if !anyExists {
			return DestinationResolution{
				RelPath:         rel,
				Suffix:          knownSuffix,
				AlreadyIngested: false,
			}
		}
	}

	for counter := 1; counter <= 10000; counter++ {
		suffix := ""
		if counter > 1 {
			suffix = fmt.Sprintf("_%d", counter)
		}
		candidateVars := vars
		candidateVars.OriginalName = SuffixedFilename(vars.OriginalName, suffix)
		rel := RenderPath(tpl, candidateVars)

		allExist, allMatch := checkRoots(roots, rel, srcPath)
		if allExist && allMatch {
			return DestinationResolution{
				RelPath:         rel,
				Suffix:          suffix,
				AlreadyIngested: true,
			}
		}
		anyExists := false
		for _, root := range roots {
			if root == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
				anyExists = true
				break
			}
		}
		if !anyExists {
			return DestinationResolution{
				RelPath:         rel,
				Suffix:          suffix,
				AlreadyIngested: false,
			}
		}
	}

	rel := RenderPath(tpl, vars)
	return DestinationResolution{RelPath: rel, Suffix: ""}
}
