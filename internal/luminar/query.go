package luminar

import (
	"fmt"
	"os"
	"strings"
)

// DefaultCatalogQuery reads one row per image out of a Luminar Neo catalog.
//
// VERIFIED against a real catalog: Luminar Neo, main_app_version 6,
// db_version 155 (macOS), 995 images, checked 2026-08-28 -- see
// docs/luminar-catalog.md for the full record. This replaces an earlier
// query built on an unconfirmed CoreData (ZASSET/ZEDIT/ZEXPORTPATH)
// hypothesis that does not match Luminar Neo's real schema at all; that
// query failed at prepare time against every real catalog, never just
// returned wrong rows.
//
// The real schema uses its own convention (_id_int_64 primary keys,
// _wide_ch text columns), and -- the load-bearing finding -- it carries NO
// relational edit->source lineage:
//   - image_user_attributes.origin_path_wide_ch is empty on every row.
//   - image_virtual_copy (a non-destructive virtual-copy link) is empty, and
//     a virtual copy is not a second file on disk regardless.
//   - img_history_states.data_wide_ch (the non-destructive edit-instruction
//     blob) has length 0 on every row; actual edit state lives in external
//     .arc/.tid/.msk/.lnp resource files, which are Luminar-internal
//     sidecars, not user-facing output files.
//   - No join table anywhere associates a derived file with its source.
//
// So this query does NOT return edit->source pairs -- it can't, because the
// catalog doesn't store them. It returns one row per (non-deleted) catalog
// image with everything derive.go's PairDerivatives needs to *infer* a pair
// from filename convention: an image whose stem plus a known suffix
// (see DefaultDerivativeSuffixes) matches another image's stem is presumed
// derived from it, corroborated (not gated) by matching EXIF.
//
// Column shape Images requires, in order (7 columns):
//   - image_id:     the catalog's own row identifier (images._id_int_64)
//   - volume_mount:  the filesystem mount point the image's volume resolves
//     to (from volumes.info_wide_ch, a JSON blob); empty if unresolvable
//   - dir_path:      the image's containing directory, relative to the
//     volume mount (paths.path_wide_ch)
//   - file_name:     the bare filename (images.path_wide_ch is NOT a full
//     path -- it is only ever a filename in this schema)
//   - trashed:        1 if the image is in Luminar's trash, else 0
//   - camera_model:  EXIF camera model, empty if unknown
//   - capture_time:  EXIF capture timestamp, falling back to the catalog's
//     own creation_date_int_64 when EXIF has none
const DefaultCatalogQuery = `
SELECT
    CAST(i._id_int_64 AS TEXT)                                                             AS image_id,
    COALESCE(json_extract(NULLIF(v.info_wide_ch, ''), '$.kMountPointSerializationKey'), '') AS volume_mount,
    p.path_wide_ch                                                                          AS dir_path,
    i.path_wide_ch                                                                          AS file_name,
    COALESCE(ua.trash_bool, 0)                                                              AS trashed,
    COALESCE(x.camera_model_wide_ch, '')                                                    AS camera_model,
    COALESCE(NULLIF(x.date_time_int_64, 0), i.creation_date_int_64)                         AS capture_time
FROM images i
JOIN paths_images pi ON pi._val_id_int_64 = i._id_int_64
JOIN paths p         ON p._id_int_64      = pi._key_id_int_64
JOIN volumes v       ON v._id_int_64      = p.volume_id_int_64
LEFT JOIN image_user_attributes ua ON ua._out_id_int_64 = i._id_int_64
LEFT JOIN image_exiv_attributes x  ON x._out_id_int_64  = i._id_int_64
WHERE i.marked_to_delete_bool = 0
  AND i.deleted_at_int_64     = 0
  AND p.marked_to_delete_bool = 0
  AND v.marked_to_delete_bool = 0
ORDER BY i._id_int_64
`

// LoadQueryFile reads a SQL query from path, for --query-file. Returns the
// file's contents verbatim (trimmed of surrounding whitespace); no
// validation beyond that is done here -- Images itself validates the
// resulting column shape once the query actually runs.
//
// NOTE: --query-file only corrects row extraction (which images the catalog
// exposes and what's known about each). It does not affect derivative
// pairing -- see DefaultDerivativeSuffixes and the -derivative-suffixes flag
// in cmd/branchdam-agent/luminarsync.go for that half of the schema-mapping
// escape hatch.
func LoadQueryFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("luminar: read query file %s: %w", path, err)
	}
	q := strings.TrimSpace(string(data))
	if q == "" {
		return "", fmt.Errorf("luminar: query file %s is empty", path)
	}
	return q, nil
}
