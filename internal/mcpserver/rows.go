package mcpserver

import "github.com/s3ntin3l8/branchdam-agent/internal/queue"

// Failed-step names. Same strings as the tray's failed-row list. This
// package does not import internal/tray.
const (
	stepArchiveCopy = "archive_copy"
	stepRebase      = "rebase"
	stepUpload      = "upload"
)

// FailedRow is one terminally failed queue row. Field names match the
// tray's FailedQueueRow JSON shape.
type FailedRow struct {
	NodeUUID    string `json:"nodeUuid"`
	FileName    string `json:"fileName"`
	SourcePath  string `json:"sourcePath"`
	LocalPath   string `json:"localPath"`
	ArchivePath string `json:"archivePath"`
	SizeBytes   int64  `json:"sizeBytes"`
	Step        string `json:"step"`
	LastError   string `json:"lastError"`
	Attempts    int    `json:"attempts"`
	CreatedAt   int64  `json:"createdAtUnix"`
}

// lookupRow is a queue row plus the three step statuses, so a pending row,
// one awaiting rebase, and a finished row are not the same shape. Step is
// only set when the row failed; the statuses are always set.
type lookupRow struct {
	FailedRow
	ArchiveCopyStatus string `json:"archiveCopyStatus"`
	RebaseStatus      string `json:"rebaseStatus"`
	UploadStatus      string `json:"uploadStatus"`
}

func lookupFrom(rec queue.Record) lookupRow {
	row := lookupRow{FailedRow: failedRow(rec)}
	row.ArchiveCopyStatus = rec.ArchiveCopyStatus
	row.RebaseStatus = rec.RebaseStatus
	row.UploadStatus = rec.UploadStatus
	if !rec.Failed() {
		row.Step = ""
		row.LastError = ""
		row.Attempts = 0
	}
	return row
}

func failedRow(rec queue.Record) FailedRow {
	row := FailedRow{
		NodeUUID:    rec.NodeUUID,
		FileName:    rec.FileName,
		SourcePath:  rec.SourcePath,
		LocalPath:   rec.LocalPath,
		ArchivePath: rec.ArchivePath,
		SizeBytes:   rec.SizeBytes,
		CreatedAt:   rec.CreatedAtUnix,
	}
	// An archive-copy failure ends the row before rebase runs, so it
	// wins when more than one column is FAILED.
	switch {
	case rec.ArchiveCopyStatus == queue.StatusFailed:
		row.Step = stepArchiveCopy
		row.LastError = rec.ArchiveCopyLastError
		row.Attempts = rec.ArchiveCopyAttempts
	case rec.UploadStatus == queue.StatusFailed:
		row.Step = stepUpload
		row.LastError = rec.UploadLastError
		row.Attempts = rec.UploadAttempts
	default:
		row.Step = stepRebase
		row.LastError = rec.RebaseLastError
		row.Attempts = rec.RebaseAttempts
	}
	return row
}
