package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// MinRebaseDwell is the minimum time Drain waits after EVENT_NODE_CREATED
// was accepted (202) before it will attempt POST /api/v1/agent/rebase for
// that same node. This closes a race that has nothing to do with the
// archive bytes: EVENT_NODE_CREATED is queued and applied asynchronously by
// branchDAM's own drainer (internal/agent.Drainer, polling every
// internal/agent.DefaultDrainInterval = 2s server-side, not importable
// cross-module hence the literal here), while /rebase is synchronous. If
// /rebase reaches the server before the Tier-0 create has been applied,
// handleAgentRebase's unknown-NodeUUID branch takes over: it inserts a
// brand new media_nodes row directly at the Tier-3 path with only the
// fields the rebase request body carries (fastHash, size, mtime -- no
// fullHash, no pHash, no promoted EXIF columns). The Tier-0 event then
// arrives, applyNodeCreated's own idempotency check finds the NodeUUID
// already exists, and returns a silent no-op -- discarding every promoted
// column permanently, with no read endpoint to even notice it happened.
// There is no agent-reachable way to ask "has the Tier-0 create been
// applied yet" (plan contract gap 5: no lookup-by-path/uuid for agents), so
// this dwell is a heuristic, not a proof -- twice the server's poll
// interval, chosen to make the race astronomically unlikely in practice
// without depending on anything the server promises. See docs/offline-queue.md.
const MinRebaseDwell = 4 * time.Second

// backoffBase/backoffCap bound the exponential backoff Drain applies to a
// step's own next-attempt time after a failure: backoffBase * 2^(attempts-1),
// capped at backoffCap. attempts=1 -> 2s, attempts=5 -> 32s, attempts=10+ ->
// capped at 5 minutes -- fast enough to recover quickly from a brief blip,
// bounded enough not to hammer a server or NAS mount that's genuinely down
// for hours.
const (
	backoffBase = 2 * time.Second
	backoffCap  = 5 * time.Minute
)

func backoffFor(attempts int, now time.Time) time.Time {
	if attempts < 1 {
		attempts = 1
	}
	delay := backoffBase
	for i := 1; i < attempts && delay < backoffCap; i++ {
		delay *= 2
	}
	if delay > backoffCap {
		delay = backoffCap
	}
	return now.Add(delay)
}

// drainClient is the subset of *branchdam.Client Drain needs -- Handshake
// (log-only, per the plan's contract gap 2) plus the two calls that
// actually change server state, so tests can substitute a fake.
type drainClient interface {
	Handshake(ctx context.Context, req branchdam.HandshakeRequest) (*branchdam.HandshakeResponse, error)
	PostNodeCreated(ctx context.Context, agentID string, payload branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error)
	Rebase(ctx context.Context, req branchdam.RebaseRequest) (*branchdam.RebaseResponse, error)
}

// DrainStats summarizes one Drain pass.
type DrainStats struct {
	HandshakeOK       bool
	ServerVersion     string
	HandshakeErr      error
	NodeCreatedSent   int
	ArchiveCopiesDone int
	RebasesDone       int
	RebasesFailed     int
	Remaining         int // rows still not Done() after this pass
}

// Drain runs exactly one pass over store's pending rows: an opportunistic
// handshake (log-only -- see HandshakeErr, never fatal to the rest of the
// pass), then node_created submission, then archive-copy attempts, then
// rebase attempts gated by MinRebaseDwell. A caller wanting continuous
// draining (the tray, or `queue-drain -watch`) calls Drain repeatedly; the
// per-row next-attempt timestamps Store persists are what makes repeated
// calls implement backoff rather than a tight retry loop -- Drain itself is
// stateless across calls.
//
// Every row's three steps are attempted independently and a failure on one
// row never aborts the pass for the others -- matching the same "log and
// continue" spirit as branchDAM's own scan pipeline and this repo's
// IngestCard.
func Drain(ctx context.Context, client drainClient, store *queue.Store, agentID string, now func() time.Time, opts ...DrainOption) (DrainStats, error) {
	if now == nil {
		now = time.Now
	}
	drainOpts := applyDrainOptions(opts)
	var stats DrainStats

	if hs, err := client.Handshake(ctx, branchdam.HandshakeRequest{AgentID: agentID}); err != nil {
		stats.HandshakeErr = err
	} else {
		stats.HandshakeOK = hs.OK
		stats.ServerVersion = hs.ServerVersion
	}

	rows, err := store.Pending(ctx)
	if err != nil {
		return stats, fmt.Errorf("ingest: drain: list pending: %w", err)
	}

	nowT := now()

	// Phase 1: node_created submissions.
	for _, r := range rows {
		if r.Kind != queue.KindMedia || r.NodeCreatedStatus != queue.StatusPending {
			continue
		}
		if r.NodeCreatedNextAttemptUnix > nowT.Unix() {
			continue
		}
		var payload branchdam.NodeCreatedPayload
		if err := json.Unmarshal([]byte(r.NodeCreatedPayloadJSON), &payload); err != nil {
			// A malformed stored payload can never succeed -- but there is
			// no FAILED state for node_created (see queue.StatusFailed's
			// doc comment); record the error and let it keep retrying
			// (harmlessly, always failing the same way) rather than
			// inventing a new terminal state this package's tests don't
			// cover. A malformed payload here would be this package's own
			// bug (it wrote the JSON), not a transient server condition.
			_ = store.MarkNodeCreatedAttempt(ctx, r.NodeUUID, fmt.Sprintf("decode stored payload: %v", err), backoffFor(r.NodeCreatedAttempts+1, nowT))
			continue
		}
		resp, err := client.PostNodeCreated(ctx, agentID, payload)
		if err != nil {
			_ = store.MarkNodeCreatedAttempt(ctx, r.NodeUUID, err.Error(), backoffFor(r.NodeCreatedAttempts+1, nowT))
			continue
		}
		if err := store.MarkNodeCreatedSubmitted(ctx, r.NodeUUID, resp.EventID, nowT); err != nil {
			continue
		}
		stats.NodeCreatedSent++
	}

	// Phase 2: archive copies -- independent of node_created's status.
	for _, r := range rows {
		if r.ArchiveCopyStatus != queue.StatusPending {
			continue
		}
		if r.ArchiveCopyNextAttemptUnix > nowT.Unix() {
			continue
		}
		var copyOpts []WriteOption
		if drainOpts.progress != nil {
			copyOpts = []WriteOption{WithProgress(func(n int64) {
				drainOpts.progress(ProgressEvent{Path: r.ArchivePath, Phase: ProgressPhaseCopying, BytesDone: n, TotalBytes: r.SizeBytes})
			})}
		}
		if err := CopyToArchive(r.LocalPath, r.ArchivePath, r.FullHash, copyOpts...); err != nil {
			_ = store.MarkArchiveCopyAttempt(ctx, r.NodeUUID, err.Error(), backoffFor(r.ArchiveCopyAttempts+1, nowT))
			continue
		}
		if err := store.MarkArchiveCopyDone(ctx, r.NodeUUID); err != nil {
			continue
		}
		stats.ArchiveCopiesDone++
	}

	// Phase 3: rebase -- MEDIA rows only (SIDECAR's rebase_status is
	// SKIPPED at insert and never becomes PENDING), gated on archive_copy
	// being DONE, node_created having been SUBMITTED, and MinRebaseDwell
	// having elapsed since submission. Re-read Pending() rather than reuse
	// the rows slice above: phases 1 and 2 may have just flipped a row's
	// status to something phase 3 needs to see (e.g. archive_copy DONE
	// moments ago in phase 2 of THIS pass -- still correctly deferred to a
	// later pass by the dwell check on node_created's submission time, not
	// by staleness of this slice).
	rows, err = store.Pending(ctx)
	if err != nil {
		return stats, fmt.Errorf("ingest: drain: re-list pending before rebase phase: %w", err)
	}
	for _, r := range rows {
		if r.Kind != queue.KindMedia || r.RebaseStatus != queue.StatusPending {
			continue
		}
		if r.ArchiveCopyStatus != queue.StatusDone || r.NodeCreatedStatus != queue.StatusSubmitted {
			continue
		}
		if r.RebaseNextAttemptUnix > nowT.Unix() {
			continue
		}
		submittedAt := time.Unix(r.NodeCreatedSubmittedAtUnix, 0)
		if nowT.Sub(submittedAt) < MinRebaseDwell {
			continue
		}

		fastHash := r.FastHash
		fullHash := r.FullHash
		_, err := client.Rebase(ctx, branchdam.RebaseRequest{
			NodeUUID:   r.NodeUUID,
			TargetPath: r.ArchiveContainerPath,
			MtimeUnix:  r.MtimeUnix,
			FileName:   r.FileName,
			FileExt:    r.FileExt,
			SizeBytes:  r.SizeBytes,
			FastHash:   &fastHash,
			FullHash:   &fullHash,
		})
		if err != nil {
			var httpErr *branchdam.HTTPError
			if errors.As(err, &httpErr) && httpErr.Classification() == branchdam.ClassificationFatal {
				_ = store.MarkRebaseFailed(ctx, r.NodeUUID, err.Error())
				stats.RebasesFailed++
				continue
			}
			_ = store.MarkRebaseAttempt(ctx, r.NodeUUID, err.Error(), backoffFor(r.RebaseAttempts+1, nowT))
			continue
		}
		if err := store.MarkRebaseDone(ctx, r.NodeUUID); err != nil {
			continue
		}
		stats.RebasesDone++
	}

	final, err := store.Pending(ctx)
	if err != nil {
		return stats, fmt.Errorf("ingest: drain: final pending count: %w", err)
	}
	stats.Remaining = len(final)

	return stats, nil
}
