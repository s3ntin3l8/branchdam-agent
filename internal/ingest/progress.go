package ingest

import "io"

// ProgressPhase distinguishes a copy from a verify byte-progress sample.
// Verify re-reads the whole file a second time (verify.go's cache-defeating
// re-read), so a progress readout covering only the copy would sit at 100%
// for roughly half the real wall time -- callers should label the two
// phases separately rather than pretending it's one continuous number.
type ProgressPhase string

const (
	ProgressPhaseCopying   ProgressPhase = "copying"
	ProgressPhaseVerifying ProgressPhase = "verifying"
)

// ProgressEvent is one byte-progress sample from a file's copy or verify
// step -- DualWrite/WriteLocal/CopyToArchive's copy, or Verify's re-read.
type ProgressEvent struct {
	// Path is the destination this progress applies to: the local
	// destination for DualWrite/WriteLocal, the archive destination for
	// CopyToArchive, or whichever path Verify was called against.
	Path       string
	Phase      ProgressPhase
	BytesDone  int64
	TotalBytes int64
}

// WriteOption configures optional behavior on DualWrite/WriteLocal/
// CopyToArchive/Verify -- currently just progress reporting. A
// functional-options shape even for one option, so every existing call
// site (3 positional args for DualWrite, 2 for Verify, etc., asserted by
// writer_test.go/parity_test.go) keeps compiling unchanged.
type WriteOption func(*writeOptions)

type writeOptions struct {
	onBytes func(int64)
}

// WithProgress reports the cumulative byte count written or read so far,
// each time the underlying copy loop's buffer flushes -- not after every
// single Write, but often enough for a live "N of M bytes" readout to feel
// responsive without per-call overhead.
func WithProgress(fn func(int64)) WriteOption {
	return func(o *writeOptions) { o.onBytes = fn }
}

func applyWriteOptions(opts []WriteOption) writeOptions {
	var o writeOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// progressWriter is a pure observer -- it never alters the byte stream,
// only reports the cumulative count written so far to onBytes. Slotted
// into an io.MultiWriter alongside the real destination(s) and hashers, or
// used directly as copyAligned's destination in Verify.
type progressWriter struct {
	total   int64
	onBytes func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.total += int64(len(b))
	p.onBytes(p.total)
	return len(b), nil
}

type progressReader struct {
	r       io.Reader
	total   int64
	onBytes func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.onBytes != nil {
		p.total += int64(n)
		p.onBytes(p.total)
	}
	return n, err
}

// DrainOption configures optional behavior on Drain -- currently just
// archive-copy progress reporting during phase 2. Like WriteOption, a
// functional-options shape so every existing Drain call site
// (cmd/branchdam-agent's queue-drain, this package's own drain_test.go,
// offline_crash_test.go) keeps compiling unchanged.
type DrainOption func(*drainOptions)

type drainOptions struct {
	progress func(ProgressEvent)
}

// WithDrainProgress reports byte-progress samples during Drain's phase 2
// archive copy (CopyToArchive) -- the "uploading to the archive" number a
// live queue-status readout wants (issue #32).
func WithDrainProgress(fn func(ProgressEvent)) DrainOption {
	return func(o *drainOptions) { o.progress = fn }
}

func applyDrainOptions(opts []DrainOption) drainOptions {
	var o drainOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
