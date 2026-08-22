// Package naming is a byte-for-byte port of branchDAM's own
// internal/naming/naming.go (commit c570690, ~108 lines, two regexes --
// see lines 47, 50, 98 of that file). Nothing under branchdam's internal/ is
// importable cross-module, so this is a copy of the algorithm, not the file
// verbatim (comments trimmed/adapted for this repo, logic and regexes
// unchanged). Only Stem is exported for M0's conformance contract; Analyze
// and Kind are carried along because Stem is defined in terms of Analyze in
// the original and splitting them would risk drift between the two copies.
package naming

import (
	"regexp"
	"strings"
)

// SuffixKind classifies the suffix Analyze stripped off a filename, if any.
type SuffixKind int

const (
	// SuffixNone means fileName's stem is the filename itself (after
	// extension/case/whitespace normalization) -- nothing was stripped.
	SuffixNone SuffixKind = iota
	// SuffixIndex means the stripped suffix was a "-N" or "(N)" duplicate
	// index (OS auto-rename, or an unpadded camera/human counter).
	SuffixIndex
	// SuffixRole means the stripped suffix was a "_edit"/"_proxy"/"_vN"/
	// " copy" derivation-role marker.
	SuffixRole
)

// indexSuffixRe matches a single trailing duplicate-index marker: "-N"
// (bounded to 1-2 digits -- an unbounded -\d+ also strips a camera's own
// hyphen-numbered default names, e.g. Sony's DSC-0001.JPG down to "dsc") or
// "(N)" (any digit count).
var indexSuffixRe = regexp.MustCompile(`(?i)(-\d{1,2}|\(\d+\))$`)

// roleSuffixRe matches a single trailing derivation-role marker.
var roleSuffixRe = regexp.MustCompile(`(?i)(_edit|_proxy|_v\d+| copy)$`)

// Analyze normalizes fileName (strip the extension at the last '.',
// lowercase, trim whitespace) and repeatedly strips one trailing
// index-or-role marker at a time. When a filename strips markers of both
// kinds ("photo-2_edit.jpg"), the returned SuffixKind is SuffixIndex: index
// ambiguity dominates over role ambiguity, since index is the one that
// changes resolver behavior in branchDAM's internal/graph.
func Analyze(fileName string) (string, SuffixKind) {
	stem := fileName
	if i := strings.LastIndex(stem, "."); i > 0 {
		stem = stem[:i]
	}
	stem = strings.ToLower(strings.TrimSpace(stem))

	kind := SuffixNone
	for {
		if stripped := indexSuffixRe.ReplaceAllString(stem, ""); stripped != stem {
			stem = stripped
			kind = SuffixIndex
			continue
		}
		if stripped := roleSuffixRe.ReplaceAllString(stem, ""); stripped != stem {
			stem = stripped
			if kind == SuffixNone {
				kind = SuffixRole
			}
			continue
		}
		break
	}
	return stem, kind
}

// Stem returns fileName's normalized filename stem, byte-identical to
// branchDAM's own naming.Stem -- this is what backs
// media_nodes.filename_stem and the payload field of the same name in
// NodeCreatedPayload.
func Stem(fileName string) string {
	stem, _ := Analyze(fileName)
	return stem
}

// Kind returns the SuffixKind Analyze classified fileName's stripped suffix
// as (SuffixNone if nothing was stripped).
func Kind(fileName string) SuffixKind {
	_, kind := Analyze(fileName)
	return kind
}
