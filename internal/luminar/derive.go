package luminar

import "strings"

// DefaultDerivativeSuffixes are the only two suffixes confirmed against a
// real Luminar Neo catalog (db_version 155): Neo writes a NEW file, added
// back to the library alongside the original, when its Upscale or Panorama
// Stitching feature runs.
//
//	IMG_1767.jpeg           -> IMG_1767_upscale.jpg
//	DJI_..._0008_D.JPG      -> DJI_..._0008_D_PANORAMA.tiff
//
// Run across all 995 images in that catalog with a deliberately WIDE
// candidate list (also including hdr/enhanced/denoise/sky-enhance/relight/
// sharpen/composite/merge/stack), only these two suffixes ever matched, and
// every match resolved to exactly one unambiguous source: zero false
// positives, zero stem collisions. Nothing else is shipped as a default on
// that basis -- a Neo feature this catalog never exercised (e.g. an HDR
// merge) is plausible but unconfirmed. An operator whose catalog has other
// derivative filenames should pass -derivative-suffixes rather than wait for
// a code change, mirroring -query-file's role for row extraction.
var DefaultDerivativeSuffixes = []string{"_upscale", "_panorama"}

// EditSourcePair is one edit->source relationship *inferred* from filename
// convention -- the catalog stores no relational lineage to read this from
// directly (see query.go). SourceRowID/EditRowID are the catalog's own row
// identifiers, stamped into the emitted edge's evidenceJson so a future
// data-correction migration (see docs/luminar-catalog.md) can find every
// edge produced by a given schema-mapping version.
type EditSourcePair struct {
	SourcePath  string
	EditPath    string
	SourceRowID string
	EditRowID   string

	// Suffix is the derivative suffix that matched (e.g. "_upscale").
	Suffix string
	// CameraModelMatch/CaptureTimeMatch are EXIF corroboration recorded as
	// evidence, never a pairing gate -- see PairDerivatives' doc comment for
	// why capture time must stay optional.
	CameraModelMatch bool
	CaptureTimeMatch bool
	SourceTrashed    bool
	EditTrashed      bool
}

// AmbiguityReason classifies why a derivative-suffix candidate did not
// produce a pair.
type AmbiguityReason int

const (
	// ReasonNoSource means no other image shares the candidate's
	// stripped-suffix stem.
	ReasonNoSource AmbiguityReason = iota
	// ReasonAmbiguous means more than one image shares it -- pairing would
	// be a guess, so nothing is emitted.
	ReasonAmbiguous
	// ReasonPathUnresolvable means the candidate or its sole stem-match has
	// no VolumeMount, so no absolute path (and therefore no nodeindex
	// lookup) can be built for it.
	ReasonPathUnresolvable
)

// Ambiguity records a derivative-suffix candidate that did NOT produce a
// pair. Reported in Stats so "0 pairs found" is distinguishable from
// "candidates existed but none were safe to pair."
type Ambiguity struct {
	FileName string
	Suffix   string
	Matches  int // count of stem matches found (0, or >1 for ReasonAmbiguous)
	Reason   AmbiguityReason
}

// stem lowercases fileName and strips its extension at the last '.'.
// Deliberately NOT internal/naming.Stem: that package is a byte-for-byte
// port of branchDAM's own naming.Stem under an explicit AGENTS.md invariant,
// its role-suffix pattern has no "_upscale"/"_panorama", and it must not
// gain Luminar-specific suffixes just because this package needs something
// similar-looking.
func stem(fileName string) string {
	s := fileName
	if i := strings.LastIndex(s, "."); i > 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// splitDerivative reports whether fileName's stem (case-insensitive) ends
// with one of suffixes and, if so, the base stem with that suffix removed
// and which suffix matched. This is a plain strings.HasSuffix check on the
// whole stem -- "_upscale" only matches at the very end of the name
// (photo_upscale_final.jpg does not match; photo_upscale.jpg does), simply
// because HasSuffix only ever looks at the tail, not because of any
// additional word-boundary logic.
func splitDerivative(fileName string, suffixes []string) (base, suffix string, ok bool) {
	s := stem(fileName)
	for _, suf := range suffixes {
		lsuf := strings.ToLower(suf)
		if lsuf == "" {
			continue
		}
		if strings.HasSuffix(s, lsuf) {
			return s[:len(s)-len(lsuf)], suf, true
		}
	}
	return "", "", false
}

// PairDerivatives infers edit->source pairs from images purely by filename
// convention: for each image whose stem ends in one of suffixes, look up
// every other image sharing the stripped-suffix stem. Exactly one match
// pairs; zero or more than one is reported as an Ambiguity and never
// emitted, so a coincidental collision can't silently mislink two unrelated
// files.
//
// The stem key deliberately ignores DirPath -- two same-named source images
// in different folders (e.g. IMG_1767.jpeg imported from two separate
// trips/cards) would make every derivative of that name ambiguous, even
// though only one folder's derivative is really its match. Acknowledged and
// left as-is: this is forward-looking (the verified catalog had zero stem
// collisions across all 995 images -- see docs/luminar-catalog.md) and the
// fail-closed design means a collision produces zero wrong edges, only a
// missed one (ReasonAmbiguous). If cross-directory same-name collisions
// ever show up in practice, key on (DirPath, base stem) instead.
//
// EXIF (camera model, capture time) is recorded as corroborating evidence on
// the resulting pair, never used to gate the match itself: the two pairs
// this rule was verified against agree on camera model, but only one of the
// two also agrees on capture time (Neo's Panorama Stitching output is
// finalized well after the source shots) -- gating on capture-time agreement
// would silently drop a true pair.
func PairDerivatives(images []CatalogImage, suffixes []string) (pairs []EditSourcePair, amb []Ambiguity) {
	byStem := make(map[string][]CatalogImage, len(images))
	for _, img := range images {
		s := stem(img.FileName)
		byStem[s] = append(byStem[s], img)
	}

	for _, img := range images {
		base, suffix, ok := splitDerivative(img.FileName, suffixes)
		if !ok {
			continue
		}
		candidates := byStem[base]
		if len(candidates) != 1 {
			reason := ReasonAmbiguous
			if len(candidates) == 0 {
				reason = ReasonNoSource
			}
			amb = append(amb, Ambiguity{FileName: img.FileName, Suffix: suffix, Matches: len(candidates), Reason: reason})
			continue
		}
		src := candidates[0]

		srcPath, srcOK := src.FullPath()
		editPath, editOK := img.FullPath()
		if !srcOK || !editOK {
			amb = append(amb, Ambiguity{FileName: img.FileName, Suffix: suffix, Matches: len(candidates), Reason: ReasonPathUnresolvable})
			continue
		}

		pairs = append(pairs, EditSourcePair{
			SourcePath:       srcPath,
			EditPath:         editPath,
			SourceRowID:      src.ImageID,
			EditRowID:        img.ImageID,
			Suffix:           suffix,
			CameraModelMatch: src.CameraModel != "" && src.CameraModel == img.CameraModel,
			CaptureTimeMatch: src.CaptureTime != 0 && src.CaptureTime == img.CaptureTime,
			SourceTrashed:    src.Trashed,
			EditTrashed:      img.Trashed,
		})
	}
	return pairs, amb
}
