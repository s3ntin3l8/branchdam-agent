package ingest

import (
	"context"
	"errors"
	"fmt"
)

// ErrCanceled is returned (wrapped) by IngestCard and IngestCardOffline when
// a stop requested through WithFileBoundaryCancel ended the walk early. It is
// distinct from context.Canceled on purpose: a parent ctx cancellation (a
// process shutdown) still reaches the file in progress, while an operator
// stop only ever takes effect between files.
var ErrCanceled = errors.New("ingest: canceled by operator")

// fileBoundaryKey is the context key carrying the stop signal installed by
// WithFileBoundaryCancel.
type fileBoundaryKey struct{}

// WithFileBoundaryCancel returns a context for IngestCard/IngestCardOffline
// and a cancel func that asks the walk to stop BEFORE the next file. The
// file being copied when cancel is called runs to completion -- copy,
// verify and registration (or queue.db row) -- under ctx's own lifetime,
// so a stop never throws away a copy that is mostly done or leaves it for
// the next run to re-verify. Cancelling ctx itself, by contrast, aborts
// DualWrite/Verify within one buffer of the file in flight (WithContext);
// that is safe -- the next run re-checks and registers what it finds -- but
// an operator asking to stop wants the work already under way kept.
//
// The returned ctx carries the stop signal as a value; it is not itself
// canceled by stop, so every network call and subprocess the in-flight file
// makes keeps ctx's own deadline and cancellation. Callers must call the
// returned cancel func when the ingest returns, as with context.WithCancel.
func WithFileBoundaryCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	stop, cancel := context.WithCancel(context.Background())
	return context.WithValue(ctx, fileBoundaryKey{}, stop), cancel
}

// stopRequested reports why a card walk must stop before its next entry:
// ctx's own error (shutdown) or ErrCanceled (an operator stop). nil means
// carry on.
func stopRequested(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stop, ok := ctx.Value(fileBoundaryKey{}).(context.Context); ok && stop.Err() != nil {
		return ErrCanceled
	}
	return nil
}

// walkStopped wraps a walk error caused by a stop -- ctx's own error, or
// ErrCanceled from stopRequested -- so the caller can keep the partial
// result: a stop is not a failure of the card, and the files already
// processed have been copied and registered or queued. Returns nil for any
// other error.
func walkStopped(ctx context.Context, cardRoot string, err error) error {
	if ctxErr := ctx.Err(); (ctxErr != nil && errors.Is(err, ctxErr)) || errors.Is(err, ErrCanceled) {
		return fmt.Errorf("ingest: card %s: stopped before the remaining files: %w", cardRoot, err)
	}
	return nil
}
