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

func buildTemplateVars(exif *ExifResult, srcPath string, modTime time.Time) TemplateVars {
	vars := TemplateVars{OriginalName: filepath.Base(srcPath)}
	if exif != nil && exif.CapturedAt != nil {
		vars.CapturedAt = *exif.CapturedAt
	} else {
		vars.CapturedAt = modTime
	}
	if exif != nil {
		vars.CameraModel = exif.CameraModel
	}
	return vars
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
	//
	// The Contains check is deliberately broader than a strict traversal
	// guard -- it also flattens a legit segment like "report..final.jpg"
	// to "_". The threat model (issue #99) is untrusted camera-card
	// content, where filenames containing ".." are vanishingly rare; the
	// conservative choice prioritizes the no-".." invariant over
	// preserving such names verbatim.
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

// collisionSampleSize is the per-region FastHash sample size used during
// the auto-suffix collision sweep in ResolveDestination (issue #105). 256KiB
// per region keeps a single hash call's read budget at ~768KiB instead of
// the canonical 6MiB, which matters when the sweep walks a directory of
// thousands of prior collisions. The full-sample 2MiB is reserved for the
// final-destination match on the very first counter (the one without a
// suffix), where confirming an AlreadyIngested identity is the operator-
// visible contract that drives the dedupe-vs-overwrite decision.
const collisionSampleSize = 256 * 1024

// hashBudget caps the total FastHash read traffic (sum of all regions
// across every filesMatch/collisionFilesMatch call in a single
// ResolveDestination) at this many bytes. 2 GiB is generous -- enough for
// the first counter's full-sample 12MiB read plus ~1360 collision-sample
// iterations at ~1.5MiB each, covering the 1000-collision benchmark in
// issue #105 with margin to spare -- while bounding the worst case to a
// single-digit-second pause instead of the unbounded 80 TB worst case
// the unfixed 10000-iteration loop permitted.
//
// On exhaustion the loop falls back to an Lstat-only walk to find the
// next free counter, never returning a DestinationResolution whose
// RelPath already exists on disk (Hermes review on PR #129: returning
// the current counter's rel on budget exhaustion would point the
// downstream createExclusive(O_EXCL) write at a path that is already
// taken once the archive carries more than ~1365 prior suffixed
// collisions, and the ingest would fail with EEXIST instead of
// allocating the next free slot).
//
// Declared as a var (not const) so tests can shrink it locally to
// force the budget-exhaustion path without provisioning 1500 colliding
// 4 MiB files.
var hashBudget int64 = 2 * 1024 * 1024 * 1024

// fastHashFile opens p and calculates its FastHash with size using the
// canonical 2MiB per-region sample. Used for the final-destination match
// (the operator-visible dedupe decision in ResolveDestination).
func fastHashFile(p string, size int64) (string, error) {
	f, err := os.Open(p) //nolint:gosec // path is our destination or source
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return hashing.FastHash(f, size)
}

// collisionFastHashFile opens p and calculates its FastHash with size using
// the smaller collisionSampleSize per-region sample. Used by
// ResolveDestination's auto-suffix sweep, where each collision only needs
// to confirm "different file" rather than "byte-identical" -- the latter
// is the full-sample hash's responsibility on the first counter.
func collisionFastHashFile(p string, size int64) (string, error) {
	f, err := os.Open(p) //nolint:gosec // path is our destination or source
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return hashing.FastHashWithSampleSize(f, size, collisionSampleSize)
}

// filesMatch reports whether dstPath exists, has the same size, and has
// the same FastHash digest as srcPath. Uses the canonical 2MiB sample --
// this is the final-destination match that drives the dedupe contract.
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

// collisionFilesMatch is the budget-bounded counterpart of filesMatch used
// by the auto-suffix collision sweep in ResolveDestination. It short-
// circuits with a single os.Lstat on dstPath: if dstPath does not exist,
// the source is automatically a fresh slot for this counter and no hash
// work is done -- this is the common case (counter increments past the
// last existing suffixed entry) and makes that case O(1) per collision.
// When dstPath does exist, it uses the smaller collisionSampleSize per-
// region FastHash rather than the full 2MiB sample; combined with the
// hashBudget guard in ResolveDestination's caller this caps the loop's
// read traffic regardless of how many prior collisions exist on disk.
func collisionFilesMatch(srcPath, dstPath string) bool {
	if _, err := os.Lstat(dstPath); err != nil {
		// dstPath absent -- a fresh slot for this counter, no hashing
		// needed. errors.Is(err, os.ErrNotExist) is implicit in any
		// non-nil err from Lstat here, but checking explicitly keeps
		// the intent obvious.
		return false
	}
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
	srcHash, err := collisionFastHashFile(srcPath, srcInfo.Size())
	if err != nil {
		return false
	}
	dstHash, err := collisionFastHashFile(dstPath, dstInfo.Size())
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

// checkCollisionRoots is the budget-bounded counterpart of checkRoots used
// by the auto-suffix sweep in ResolveDestination. It mirrors checkRoots's
// contract -- all non-empty roots must exist and match the source for
// allMatch -- but routes through collisionFilesMatch so each comparison
// does the cheap Lstat short-circuit (O(1) when the destination is
// absent) and the small-sample FastHash (when present), keeping the loop
// inside hashBudget.
func checkCollisionRoots(roots []string, relPath, srcPath string) (allExist bool, allMatch bool) {
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
		if !collisionFilesMatch(srcPath, p) {
			return true, false
		}
	}
	return validRoots > 0, true
}

// ResolveDestination determines the relative destination path for srcPath under
// the provided roots, resolving any naming collisions by auto-suffixing (_2, _3, ...)
// and detecting whether an identical copy has already been ingested.
//
// The collision loop is bounded by hashBudget: the first counter (no
// suffix, the path the new file would actually take) uses the canonical
// 2MiB FastHash sample and the full filesMatch contract; subsequent
// counters (the auto-suffix sweep) use checkCollisionRoots, which Lstat-
// short-circuits absent destinations (the common case) and applies a
// smaller-sample FastHash (256KiB per region) when the destination is
// present. Together this bounds the loop's read traffic regardless of how
// many prior collisions exist on disk (issue #105), while preserving
// the operator-visible contract that the *first* counter's match is
// byte-identical with what's already on disk.
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

	// Bytes a single comparison against one destination costs, per
	// FastHash call. src+dst = 2; one FastHash samples 3 regions of
	// sampleSize bytes each. Used by the budget guard below.
	const (
		fullBytesPerIter     = int64(hashing.FastHashSampleSize) * 3 * 2
		collisionBytesPerItr = int64(collisionSampleSize) * 3 * 2
	)
	hashesDone := int64(0)

	for counter := 1; counter <= 10000; counter++ {
		suffix := ""
		if counter > 1 {
			suffix = fmt.Sprintf("_%d", counter)
		}
		candidateVars := vars
		candidateVars.OriginalName = SuffixedFilename(vars.OriginalName, suffix)
		rel := RenderPath(tpl, candidateVars)

		// counter==1 is the final-destination match -- the operator-
		// visible dedupe decision gets the full 2MiB sample so the
		// byte-identity confirmation against an existing identical
		// copy is canonical.
		// counter>1 is the auto-suffix sweep -- budget-bounded via
		// checkCollisionRoots (Lstat short-circuit + small-sample
		// FastHash) so the read traffic stays inside hashBudget even
		// when thousands of suffixed entries exist.
		var (
			allExist, allMatch bool
			budgetExhausted    bool
		)
		if counter == 1 {
			if hashesDone+fullBytesPerIter > hashBudget {
				// Even the first match is over budget -- an
				// over-budget archive is pathological. Defer
				// to the Lstat-only fallback walk below
				// (findNextFreeSuffix) which never returns a
				// path that already exists on disk.
				budgetExhausted = true
			} else {
				allExist, allMatch = checkRoots(roots, rel, srcPath)
				hashesDone += fullBytesPerIter
			}
		} else {
			if hashesDone+collisionBytesPerItr > hashBudget {
				budgetExhausted = true
			} else {
				allExist, allMatch = checkCollisionRoots(roots, rel, srcPath)
				hashesDone += collisionBytesPerItr
			}
		}

		if !budgetExhausted {
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
	}

	// Fallback path: either the loop ran out of counters (counter >
	// 10000) or the FastHash budget was exhausted mid-sweep. In both
	// cases the budget is no longer protecting us from taking an
	// already-occupied suffix, so use an Lstat-only walk to find the
	// next free slot. Returning a RelPath that already exists on disk
	// would surface downstream as an O_EXCL EEXIST on createExclusive
	// -- silent corruption-of-the-next-attempt rather than a graceful
	// failure (Hermes review on PR #129).
	return findNextFreeSuffix(roots, tpl, vars)
}

// findNextFreeSuffix walks the counter space with Lstat only (no
// FastHash) starting at counter=1 and returns the first slot whose
// destination does not exist in any root. Bypassed under normal
// operation by the FastHash loop above; reached only when hashBudget
// has been exhausted or the 10000-iteration cap is hit. AlreadyIngested
// is always false here -- the budget was spent without confirming
// byte-identity, so we cannot claim a duplicate-match.
func findNextFreeSuffix(roots []string, tpl string, vars TemplateVars) DestinationResolution {
	for counter := 1; counter <= 10000; counter++ {
		suffix := ""
		if counter > 1 {
			suffix = fmt.Sprintf("_%d", counter)
		}
		candidateVars := vars
		candidateVars.OriginalName = SuffixedFilename(vars.OriginalName, suffix)
		rel := RenderPath(tpl, candidateVars)
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
			return DestinationResolution{RelPath: rel, Suffix: suffix, AlreadyIngested: false}
		}
	}
	// Worst case: every counter 1..10000 is occupied. Return the
	// unsuffixed render so the caller surfaces a clear downstream
	// failure rather than silently picking something arbitrary.
	rel := RenderPath(tpl, vars)
	return DestinationResolution{RelPath: rel, Suffix: ""}
}
