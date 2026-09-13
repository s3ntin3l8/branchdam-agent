package resolve

import (
	"fmt"
	"os"
	"strings"
)

// DefaultTimelineQuery maps Resolve timeline clips to their original file
// paths. Verified against a live DaVinci Resolve 19 Project Server instance
// (PostgreSQL 13, "Norway 2025" project, 4 timelines, 365 media pool items).
//
// The query follows the relationship chain:
//
//	SM_Project → Sm2Timeline → Sm2Sequence → Sm2TiTrack → Sm2TiItem
//	                                                       ↓
//	                                                  MediaFilePath
//
// MediaFilePath contains original Windows paths (e.g.
// "D:\Videos\Norway 2025\Pixel\PXL_001.mp4") that need path rewriting
// to map to branchDAM storage tiers.
//
// Identifiers are double-quoted ("Name", "MediaFilePath", etc.) because
// Resolve's schema uses mixed-case identifiers that would be folded to
// lowercase by PostgreSQL's default identifier folding.
//
// Column shape TimelineClips requires, in order (8 columns):
//   - timeline_name:  Sm2Timeline.Name
//   - clip_name:      Sm2TiItem.Name
//   - media_file_path: Sm2TiItem.MediaFilePath (the file path)
//   - in_point:       Sm2TiItem.In (source in-point, nullable)
//   - start_frame:    Sm2TiItem.Start (timeline position, nullable)
//   - duration:       Sm2TiItem.Duration (clip duration, nullable)
//   - item_id:        Sm2TiItem.Sm2TiItem_id (primary key)
//   - timeline_id:    Sm2Timeline.Sm2Timeline_id (primary key)
const DefaultTimelineQuery = `
SELECT
    t."Name"      AS timeline_name,
    i."Name"      AS clip_name,
    i."MediaFilePath" AS media_file_path,
    i."In"        AS in_point,
    i."Start"     AS start_frame,
    i."Duration"  AS duration,
    i."Sm2TiItem_id" AS item_id,
    t."Sm2Timeline_id" AS timeline_id
FROM "Sm2TiItem" i
JOIN "Sm2TiTrack" tr ON i."Sm2TiTrack_id" = tr."Sm2TiTrack_id"
JOIN "Sm2Sequence" seq ON tr."Sequence" = seq."Sm2Sequence_id"
JOIN "Sm2Timeline" t ON seq."Sm2Timeline_id" = t."Sm2Timeline_id"
WHERE i."MediaFilePath" IS NOT NULL
`

// LoadQueryFile reads a SQL query from path, for --query-file override.
// Returns the file's contents verbatim (trimmed of surrounding whitespace);
// no validation beyond that is done here — TimelineClips itself validates
// the resulting column shape once the query actually runs.
func LoadQueryFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("resolve: read query file %s: %w", path, err)
	}
	q := strings.TrimSpace(string(data))
	if q == "" {
		return "", fmt.Errorf("resolve: query file %s is empty", path)
	}
	return q, nil
}
