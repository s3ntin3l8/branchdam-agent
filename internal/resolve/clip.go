package resolve

import "database/sql"

// TimelineClip is one row of DefaultTimelineQuery's output: everything
// known about a single clip in a Resolve timeline, before any path rewriting
// or node-index resolution happens. The same file may appear in multiple
// timelines (or multiple times in the same timeline), so callers should
// deduplicate by MediaFilePath if they only care about unique files.
type TimelineClip struct {
	TimelineName  string
	ClipName      string
	MediaFilePath string // Windows path, e.g. "D:\Videos\Norway 2025\Pixel\PXL_001.mp4"
	InPoint       sql.NullString
	StartFrame    sql.NullString
	Duration      sql.NullString
	ItemID        string
	TimelineID    string
}
