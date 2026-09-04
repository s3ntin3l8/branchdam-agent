// Package exiftool pools long-lived exiftool subprocesses that speak the
// documented "-stay_open" stdin/stdout protocol, so a card ingest of
// thousands of files pays exiftool's ~50-150ms process-startup cost once
// per pooled worker instead of once per file. Callers get back exactly the
// stdout bytes a classic one-shot `exiftool <args>` invocation would have
// produced for the same args; parsing stays with the caller (internal/ingest
// and internal/phash each want different tags out of it).
package exiftool

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"
)

// readyMarker is exiftool's fixed -stay_open sentinel: the exact line it
// writes to stdout after finishing one "-execute"-terminated request.
const readyMarker = "{ready}"

// closeTimeout bounds how long Pool waits for a pooled process to exit
// gracefully (after asking it to via "-stay_open\nFalse\n") before killing
// it outright. Needed because a wedged exiftool process would otherwise
// hang graceful shutdown forever -- see NeedsSeparator's doc comment for a
// concrete, empirically-confirmed way a pooled process can wedge.
const closeTimeout = 2 * time.Second

// NeedsSeparator reports whether path is safe to send as-is through the
// pooled, line-oriented "-stay_open" argfile protocol, where each argument
// is its own line, read the same way exiftool's "-@ ARGFILE" option reads
// one: false means "path round-trips through that encoding unchanged,"
// true means "route this one through the one-shot fork instead" (see
// Pool.Execute/containsSeparator). Three documented argfile behaviors
// (exiftool's own -@ option docs) break that round-trip:
//   - a line starting with "-" is parsed as a flag or tag assignment,
//   - a line starting with "#" is skipped as a comment, silently dropping
//     the request that path was meant to be part of,
//   - leading whitespace on a line is stripped, silently changing the
//     path.
//
// A path containing "\n" or "\r" also fails the round-trip regardless of
// its own leading character, by splitting into two argfile lines -- e.g. a
// file literally named "foo\n-overwrite_original" turns its own second
// half into one of the three cases above. A plain one-shot argv, by
// contrast, is immune to all of this: exec doesn't re-parse an argv
// element as a line of anything. Callers building argv for Pool should
// only add "--" when this returns true.
//
// The "-" case isn't just an optimization: a bare "--" sent through the
// "-stay_open" protocol permanently wedges the persistent process for the
// rest of its life. Confirmed empirically against exiftool 12.76 -- once
// "--" is read from the argfile/stdin stream, exiftool treats *everything*
// it reads afterward as a literal filename, including a subsequent
// "-execute" line, which is exiftool's only signal to actually run a
// request and respond with "{ready}". A batch is a no-op until "-execute"
// is recognized, so once "--" has been sent, every later batch on that same
// process blocks forever. Pool.Execute defends against this by forking a
// one-shot process (unaffected -- it exits right after) for any request
// whose args contain "--", so a path that genuinely needs the separator
// still gets it, just without poisoning the pool. The "#"/whitespace cases
// are routed to the same fork path for a different reason: "--" doesn't
// stop exiftool's argfile line-parser from treating a line as a comment or
// trimming it, so protecting those two only works by skipping the argfile
// protocol entirely, not by adding "--" within it.
func NeedsSeparator(path string) bool {
	if path == "" {
		return false
	}
	if strings.ContainsAny(path, "\n\r") {
		return true
	}
	return path[0] == '-' || path[0] == '#' || path[0] == ' ' || path[0] == '\t'
}

// Pool manages a sync.Pool of persistent `exiftool -stay_open True -@ -`
// subprocesses for one resolved binary path. A broken or dead process is
// never returned to the pool -- the next Execute call transparently starts
// a replacement. If starting or talking to a pooled process fails outright,
// Execute falls back to a plain one-shot fork of the same args (also used,
// always, for any request containing "--" -- see NeedsSeparator), so a
// crashing or wedged exiftool degrades ingest throughput rather than
// blocking it.
type Pool struct {
	path string
	pool sync.Pool

	// mu/all track every process this Pool has ever started, independent
	// of sync.Pool's own bookkeeping. sync.Pool may silently drop an idle
	// item during GC without calling anything on it (fine for pure-memory
	// objects; see startProcess's finalizer comment for why that's not
	// fine here), so Close draining sync.Pool alone can't be trusted to
	// reap everything -- but a *strong* all would just as silently defeat
	// that same finalizer, by keeping every process reachable (and
	// therefore un-GC-able) for the Pool's entire lifetime regardless of
	// whether sync.Pool itself already dropped it. weak.Pointer lets Close
	// still deterministically reach and close whatever is currently alive
	// (idle in the pool or checked out) without preventing GC -- and hence
	// the finalizer -- from reclaiming anything sync.Pool evicts first.
	mu  sync.Mutex
	all []weak.Pointer[process]
}

// NewPool returns a Pool bound to the resolved exiftool binary at path.
// path must be non-empty and already resolved (e.g. via exec.LookPath) --
// Pool does no discovery of its own.
func NewPool(path string) *Pool {
	return &Pool{path: path}
}

// Path returns the exiftool binary path this Pool was constructed with.
func (p *Pool) Path() string { return p.path }

// Execute runs exiftool with args (e.g. []string{"-j", "-n", "-G", path})
// -- omitting the "-execute" protocol terminator, which Execute appends
// itself for the pooled path -- and returns exactly the stdout bytes
// exiftool wrote, mirroring what a one-shot `exiftool <args>` invocation
// would have produced. Falls back to a one-shot fork, with a slog.Warn,
// when the pool can't produce a usable process or a pooled process errors
// mid-request; always uses the fork path (silently, no warning -- this is
// the expected, safe route, not a degraded one) when args contains "--".
func (p *Pool) Execute(ctx context.Context, args []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if containsSeparator(args) {
		return p.executeFork(ctx, args)
	}

	proc, err := p.get()
	if err != nil {
		slog.Warn("exiftool: could not start pooled process, falling back to per-file fork", "path", p.path, "err", err)
		return p.executeFork(ctx, args)
	}

	out, err := proc.execute(ctx, args)
	if err != nil {
		proc.close()
		slog.Warn("exiftool: pooled process failed, falling back to per-file fork", "path", p.path, "err", err)
		return p.executeFork(ctx, args)
	}
	p.pool.Put(proc)
	return out, nil
}

// Close terminates and reaps every process this Pool ever started that is
// still alive, including ones currently checked out via Execute (close is
// idempotent, so a process already back in service is simply killed
// early). A process sync.Pool already evicted and the runtime already
// collected is not "still alive" -- its finalizer already ran, or will
// shortly. Safe to call even if nothing was ever pooled.
func (p *Pool) Close() {
	p.mu.Lock()
	all := p.all
	p.all = nil
	p.mu.Unlock()
	for _, wp := range all {
		if proc := wp.Value(); proc != nil {
			proc.close()
		}
	}
}

func containsSeparator(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return true
		}
	}
	return false
}

func (p *Pool) get() (*process, error) {
	if v := p.pool.Get(); v != nil {
		return v.(*process), nil
	}
	proc, err := startProcess(p.path)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.all = append(p.all, weak.Make(proc))
	p.mu.Unlock()
	return proc, nil
}

func (p *Pool) executeFork(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.path, args...) //nolint:gosec // path is a resolved/configured exiftool binary, args are caller-built
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("exiftool fork %v: %w (stderr: %s)", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// process is one persistent `exiftool -stay_open True -@ -` subprocess.
type process struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	stderr    *syncBuffer
	closeOnce sync.Once
}

func startProcess(path string) (*process, error) {
	cmd := exec.Command(path, "-stay_open", "True", "-@", "-") //nolint:gosec // path is a resolved/configured exiftool binary
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("exiftool: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("exiftool: stdout pipe: %w", err)
	}
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exiftool: start: %w", err)
	}
	pr := &process{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: &stderr}
	// sync.Pool may silently drop a Put-back item during GC without ever
	// calling anything on it -- fine for pure-memory objects, a leaked OS
	// process otherwise (and, per NeedsSeparator's doc comment, one that
	// can't even be relied on to exit on its own). The finalizer is the
	// backstop for that path; close() cancels it once a process is reaped
	// deliberately, so the common case never touches the GC finalizer
	// queue at all.
	runtime.SetFinalizer(pr, (*process).finalize)
	return pr, nil
}

// finalize is the GC backstop for a process sync.Pool silently evicted
// (see Pool.all's doc comment): killed outright rather than asked nicely,
// since there's no caller left to wait out closeTimeout. Still reaps it
// (in its own goroutine, since a finalizer must not block) so it doesn't
// trade a leaked process for a zombie one.
func (pr *process) finalize() {
	if pr.cmd.Process == nil {
		return
	}
	_ = pr.cmd.Process.Kill()
	go func() { _ = pr.cmd.Wait() }()
}

// execute writes args one per line followed by "-execute", then reads
// stdout up to (not including) the "{ready}" sentinel line -- the
// documented -stay_open request/response boundary. If ctx is done first,
// execute returns ctx.Err() without waiting for the response; the read
// goroutine it left running exits on its own once the process is
// eventually closed (Pool.Execute never returns an errored process to the
// pool, so this process is never reused while that goroutine is live).
func (pr *process) execute(ctx context.Context, args []string) ([]byte, error) {
	var req bytes.Buffer
	for _, a := range args {
		req.WriteString(a)
		req.WriteByte('\n')
	}
	req.WriteString("-execute\n")
	if _, err := pr.stdin.Write(req.Bytes()); err != nil {
		return nil, fmt.Errorf("exiftool: write request: %w", err)
	}

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var out bytes.Buffer
		for {
			line, err := pr.stdout.ReadString('\n')
			if err != nil {
				ch <- result{nil, fmt.Errorf("exiftool: read response: %w (stderr: %s)", err, pr.stderr.String())}
				return
			}
			if strings.TrimRight(line, "\r\n") == readyMarker {
				ch <- result{out.Bytes(), nil}
				return
			}
			out.WriteString(line)
		}
	}()

	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// close asks the process to stop stay_open, then reaps it, killing it
// outright if it doesn't exit within closeTimeout. Idempotent -- Pool.Close
// and Pool.Execute's error path can both end up calling close on the same
// process, and a process that already died is handled the same way (the
// stdin write/close simply errors, and Wait still reaps whatever exit
// state the OS already recorded).
func (pr *process) close() {
	pr.closeOnce.Do(pr.closeOnceFn)
}

func (pr *process) closeOnceFn() {
	runtime.SetFinalizer(pr, nil)
	_, _ = pr.stdin.Write([]byte("-stay_open\nFalse\n"))
	_ = pr.stdin.Close()

	done := make(chan struct{})
	go func() {
		_ = pr.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeTimeout):
		if pr.cmd.Process != nil {
			_ = pr.cmd.Process.Kill()
		}
		<-done
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer: cmd.Stderr is written to by
// exec's internal copy goroutine concurrently with process.execute reading
// it for diagnostics, and bytes.Buffer alone isn't safe for that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
