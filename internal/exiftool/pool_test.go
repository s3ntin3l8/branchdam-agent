package exiftool

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func requireExiftool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not found on PATH -- skipping real-binary exiftool pool test")
	}
	return path
}

// TestPoolExecuteReusesProcess proves the pooled path actually talks to a
// long-lived process rather than forking: two Execute calls in a row must
// not fork twice (fork would be observable as two separate exiftool
// startups, which -ver's own output can't distinguish -- so instead this
// asserts on functional correctness of the protocol round-trip, which is
// the load-bearing thing pooling must not break).
func TestPoolExecuteReusesProcess(t *testing.T) {
	path := requireExiftool(t)
	pool := NewPool(path)
	defer pool.Close()

	for i := 0; i < 3; i++ {
		out, err := pool.Execute(context.Background(), []string{"-ver"})
		if err != nil {
			t.Fatalf("Execute call %d: %v", i, err)
		}
		if !strings.HasPrefix(string(out), "1") {
			t.Errorf("Execute call %d = %q, want exiftool version output", i, out)
		}
	}
}

// TestPoolExecuteWithSeparatorForksInsteadOfPoisoningPool proves the
// documented "--" wedge (see NeedsSeparator's doc comment) never reaches
// the pooled process: a request containing "--" must still succeed (via
// the fork fallback), and a subsequent pooled request on the same Pool
// must also still succeed -- i.e. the pool itself was never handed the
// poisoned request.
func TestPoolExecuteWithSeparatorForksInsteadOfPoisoningPool(t *testing.T) {
	path := requireExiftool(t)
	pool := NewPool(path)
	defer pool.Close()

	if _, err := pool.Execute(context.Background(), []string{"-ver", "--"}); err != nil {
		t.Fatalf("Execute with -- : %v", err)
	}

	out, err := pool.Execute(context.Background(), []string{"-ver"})
	if err != nil {
		t.Fatalf("pooled Execute after a -- request: %v", err)
	}
	if !strings.HasPrefix(string(out), "1") {
		t.Errorf("Execute after -- = %q, want exiftool version output", out)
	}
}

// TestPoolExecuteContextCanceled proves an already-canceled context fails
// fast rather than talking to the process at all.
func TestPoolExecuteContextCanceled(t *testing.T) {
	path := requireExiftool(t)
	pool := NewPool(path)
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pool.Execute(ctx, []string{"-ver"}); err == nil {
		t.Error("Execute with a canceled context = nil error, want context.Canceled")
	}
}

// TestPoolFasterThan100FreshForks is the AC-required perf proof: 100
// requests through a Pool (one persistent process, reused) must be faster
// than 100 fresh `exiftool -ver` subprocess invocations, since the whole
// point of pooling is amortizing exiftool's ~50-150ms process-startup cost
// across many calls instead of paying it per call. "-ver" is used as the
// tiny fixture-free request so the test itself stays fast and has nothing
// to do with file I/O.
func TestPoolFasterThan100FreshForks(t *testing.T) {
	path := requireExiftool(t)
	const n = 100

	pool := NewPool(path)
	defer pool.Close()
	// Warm the pool once so its startup cost isn't counted against it --
	// the whole premise being tested is amortized *steady-state* cost, not
	// first-call latency, which a single fresh fork also pays.
	if _, err := pool.Execute(context.Background(), []string{"-ver"}); err != nil {
		t.Fatalf("warmup Execute: %v", err)
	}

	pooledStart := time.Now()
	for i := 0; i < n; i++ {
		if _, err := pool.Execute(context.Background(), []string{"-ver"}); err != nil {
			t.Fatalf("pooled Execute %d: %v", i, err)
		}
	}
	pooledElapsed := time.Since(pooledStart)

	forkStart := time.Now()
	for i := 0; i < n; i++ {
		if err := exec.Command(path, "-ver").Run(); err != nil {
			t.Fatalf("fresh fork %d: %v", i, err)
		}
	}
	forkElapsed := time.Since(forkStart)

	t.Logf("%d pooled calls: %v; %d fresh forks: %v", n, pooledElapsed, n, forkElapsed)
	if pooledElapsed >= forkElapsed {
		t.Errorf("pooled %d calls took %v, want faster than %d fresh forks (%v)", n, pooledElapsed, n, forkElapsed)
	}
}

// TestPoolFinalizerReapsGCEvictedProcess is a regression test for a real
// bug caught in review: an earlier version of Pool tracked every started
// process via a *strong* reference (Pool.all []*process) so Close could
// deterministically reap them. That defeated the whole point of the
// finalizer in startProcess -- a process sync.Pool silently evicted mid-
// lifetime was still reachable via Pool.all, so it was never garbage
// collected, so its finalizer never ran, so it just... stayed alive,
// unreaped, until Close. Pool.all now holds weak.Pointer[process] instead,
// specifically so this scenario is possible to test: put a process back
// via Execute, drop every OTHER reference to it (including this test's
// own), force two GC cycles (what it actually takes to clear a sync.Pool:
// the first moves local pools to the "victim" cache, the second clears
// it), and confirm the OS process actually exits -- proving the finalizer
// ran, not just that some Go-level bookkeeping looks right.
func TestPoolFinalizerReapsGCEvictedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-liveness check uses a POSIX signal(0) probe")
	}
	path := requireExiftool(t)
	pool := NewPool(path)
	defer pool.Close()

	if _, err := pool.Execute(context.Background(), []string{"-ver"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	pid, ok := pooledProcessPID(t, pool)
	if !ok {
		t.Fatal("process already collected before the test could observe its PID")
	}
	// pid is now the only thing this test still holds -- the strong
	// *process reference pooledProcessPID resolved via weak.Value() went
	// out of scope with that call, so it's not keeping anything reachable
	// here; see the func doc comment for why that matters.

	runtime.GC()
	runtime.GC()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("pid %d still running 5s after the last strong reference was dropped and two GC cycles ran -- the finalizer did not reap it", pid)
}

// pooledProcessPID reads the PID of pool's single tracked process without
// leaking a strong reference back to the caller -- returning an int
// instead of *process means the *process itself goes out of scope when
// this function returns, rather than living on in a caller-held variable.
func pooledProcessPID(t *testing.T, pool *Pool) (int, bool) {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.all) != 1 {
		t.Fatalf("pool.all = %d entries, want 1", len(pool.all))
	}
	proc := pool.all[0].Value()
	if proc == nil {
		return 0, false
	}
	return proc.cmd.Process.Pid, true
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
