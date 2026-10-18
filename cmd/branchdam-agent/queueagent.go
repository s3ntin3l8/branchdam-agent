package main

import (
	"context"
	"io"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/prune"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// queueCountsReader implements tray.QueueReader over a real *queue.Store --
// the concrete wiring cmd/branchdam-agent owns, matching configSettings'
// own relationship to tray.Settings (internal/tray never imports
// internal/queue).
type queueCountsReader struct {
	store *queue.Store
}

func (q *queueCountsReader) Counts(ctx context.Context) (tray.QueueCounts, error) {
	c, err := q.store.Counts(ctx)
	if err != nil {
		return tray.QueueCounts{}, err
	}
	return tray.QueueCounts{
		AwaitingUpload: c.AwaitingUpload,
		AwaitingRebase: c.AwaitingRebase,
		Failed:         c.Failed,
		Done:           c.Done,
		PendingBytes:   c.PendingBytes,
	}, nil
}

// FailedRows implements tray.QueueFailureManager over queue.Store's own
// FailedRows, for the window's failed-rows list.
func (q *queueCountsReader) FailedRows(ctx context.Context, limit int) ([]tray.FailedQueueRow, error) {
	recs, err := q.store.FailedRows(ctx, limit)
	if err != nil {
		return nil, err
	}
	rows := make([]tray.FailedQueueRow, 0, len(recs))
	for _, rec := range recs {
		row := tray.FailedQueueRow{
			NodeUUID:    rec.NodeUUID,
			FileName:    rec.FileName,
			SourcePath:  rec.SourcePath,
			LocalPath:   rec.LocalPath,
			ArchivePath: rec.ArchivePath,
			SizeBytes:   rec.SizeBytes,
			CreatedAt:   rec.CreatedAtUnix,
		}
		// An archive-copy failure ends the row before rebase ever runs
		// (queue.Record.Done), so it is the step to report when both
		// columns somehow read FAILED.
		switch {
		case rec.ArchiveCopyStatus == queue.StatusFailed:
			row.Step = tray.FailedStepArchiveCopy
			row.LastError = rec.ArchiveCopyLastError
			row.Attempts = rec.ArchiveCopyAttempts
		case rec.UploadStatus == queue.StatusFailed:
			row.Step = tray.FailedStepUpload
			row.LastError = rec.UploadLastError
			row.Attempts = rec.UploadAttempts
		default:
			row.Step = tray.FailedStepRebase
			row.LastError = rec.RebaseLastError
			row.Attempts = rec.RebaseAttempts
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// RetryFailed implements tray.QueueFailureManager: the same reset
// `queue-drain -retry-failed` runs (queue.Store.ResetFailed) for every row
// when nodeUUID is "", or queue.Store.ResetFailedNode for one.
func (q *queueCountsReader) RetryFailed(ctx context.Context, nodeUUID string) (tray.QueueRetryCounts, error) {
	var (
		c   queue.ResetCounts
		err error
	)
	if nodeUUID == "" {
		c, err = q.store.ResetFailed(ctx)
	} else {
		c, err = q.store.ResetFailedNode(ctx, nodeUUID)
	}
	if err != nil {
		return tray.QueueRetryCounts{}, err
	}
	return tray.QueueRetryCounts{ArchiveCopies: c.ArchiveCopies, Rebases: c.Rebases, Uploads: c.Uploads}, nil
}

// queueDrainer implements tray.Drainer over internal/ingest.Drain -- the
// same function `queue-drain`'s own runDrainPass calls, so the tray's
// timer-driven and menu-driven drain passes behave identically to the
// headless subcommand.
type queueDrainer struct {
	client  *branchdam.Client
	store   *queue.Store
	agentID string
	// uploadCtx is the tray's own lifetime, which bounds queued uploads
	// instead of the 2-minute pass deadline (ingest.WithUploadContext). Nil
	// -- a test, or a caller that never set it -- falls back to the pass's
	// context.
	uploadCtx context.Context //nolint:containedctx // the runner's lifetime, handed to Drain as an option
	// uploadGate is asked before each queued upload (ingest.WithUploadGate)
	// so a link that turns metered mid-pass holds the rest. The tray passes
	// Runner.UploadGate. Nil sends every queued upload.
	uploadGate func() (metered bool, err error)
}

func (d *queueDrainer) Drain(ctx context.Context) (tray.DrainSummary, error) {
	var opts []ingest.DrainOption
	if d.uploadCtx != nil {
		opts = append(opts, ingest.WithUploadContext(d.uploadCtx))
	}
	if d.uploadGate != nil {
		opts = append(opts, ingest.WithUploadGate(d.uploadGate))
	}
	stats, err := ingest.Drain(ctx, d.client, d.store, d.agentID, nil, opts...)
	var lastHandshakeAt time.Time
	if stats.HandshakeOK {
		lastHandshakeAt = time.Now()
	}
	return tray.DrainSummary{
		HandshakeOK:         stats.HandshakeOK,
		LastHandshakeAt:     lastHandshakeAt,
		NodeCreatedSent:     stats.NodeCreatedSent,
		ArchiveCopiesDone:   stats.ArchiveCopiesDone,
		ArchiveCopiesFailed: stats.ArchiveCopiesFailed,
		RebasesDone:         stats.RebasesDone,
		RebasesFailed:       stats.RebasesFailed,
		UploadsDone:         stats.UploadsDone,
		UploadsFailed:       stats.UploadsFailed,
		Remaining:           stats.Remaining,
	}, err
}

// queuePruner implements tray.Pruner over internal/prune.Pass -- never in
// dry-run mode, since a timer-driven or menu-driven tray pass is always a
// real "actually free this disk space" action, matching `prune`'s own
// default (non -dry-run) behavior. Per-file [PRUNED]/[SKIP]/[ERROR] lines
// go to io.Discard: there is no console for a -H windowsgui-linked tray to
// write them to, and Stats is what the menu/status page show instead.
type queuePruner struct {
	client *branchdam.Client
	store  *queue.Store
	cfg    config.Config
}

func (p *queuePruner) Prune(ctx context.Context) (tray.PruneSummary, error) {
	stats, err := prune.Pass(ctx, io.Discard, p.client, p.store, p.cfg, time.Now(), false)
	return tray.PruneSummary{
		Evaluated:  stats.Evaluated,
		Pruned:     stats.Pruned,
		FreedBytes: stats.FreedBytes,
	}, err
}

// startPeriodic calls fn on a ticker every interval, each call bounded by
// timeout, until ctx is cancelled. Mirrors selfUpdateAgent.Run's own
// ticker shape (selfupdateagent.go) -- the tray's drain and prune timers
// are the second and third independent background loops run this way,
// decoupled entirely from internal/tray/run_supported.go's menu-refresh
// select loop (see tray.Runner.TriggerDrain/TriggerPrune's doc comments
// for why their own locking is independent of Runner.gate). interval <= 0
// disables the loop -- callers only start this goroutine when a Drainer/
// Pruner is actually configured, but the guard is kept here too so a
// future caller can't forget it.
func startPeriodic(ctx context.Context, interval, timeout time.Duration, fn func(context.Context)) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fnCtx, cancel := context.WithTimeout(ctx, timeout)
			fn(fnCtx)
			cancel()
		}
	}
}

// periodicPassTimeout bounds each timer-driven drain/prune pass -- matching
// queue-drain's/prune's own default -timeout flag, so a tray-driven pass
// can't hang indefinitely on a wedged network call or NAS mount.
const periodicPassTimeout = 2 * time.Minute
