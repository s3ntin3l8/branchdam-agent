package tray

import (
	"log/slog"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/netgate"
)

// meteredProbeTTL bounds how often the display path re-runs the metered
// probe. Meteredness changes when the operator switches network, not
// second to second; the drain gate itself never reads this cache (see
// TriggerDrain), so the TTL only affects how quickly the tooltip notices.
const meteredProbeTTL = 30 * time.Second

// meteredCache is the display-side copy of netgate.IsMetered's answer.
// The probe can shell out (nmcli on Linux) or call into the OS network
// stack, either of which can stall; Status() runs on the tray's menu loop
// and on every status-page request, so it must never call the probe
// inline. meteredUploadPaused reads this cache and kicks a background
// refresh when it is stale, mirroring scratchUsageSnapshot.
//
// This cache is for display only. TriggerDrain still runs the probe
// synchronously on its own (non-UI) goroutine and treats an error as
// metered, so the drain gate's fail-closed guarantee (AGENTS.md invariant
// 15) never depends on a cached or not-yet-probed value.
type meteredCache struct {
	mu       sync.Mutex
	probed   bool
	metered  bool
	err      error
	at       time.Time
	inFlight bool
}

// meteredUploadPaused reports whether the tray should show uploads as
// paused for a metered connection: pause-on-metered is enabled and the
// most recent probe said metered or failed (shown fail-closed, matching
// the drain gate). Never blocks on the probe; before the first probe
// completes it reports false.
func (r *Runner) meteredUploadPaused(pauseOnMetered bool) bool {
	if !pauseOnMetered {
		return false
	}
	c := &r.meteredCache
	c.mu.Lock()
	fresh := c.probed && time.Since(c.at) < meteredProbeTTL
	paused := c.probed && (c.metered || c.err != nil)
	start := !fresh && !c.inFlight
	if start {
		c.inFlight = true
	}
	c.mu.Unlock()
	if start {
		go r.refreshMetered()
	}
	return paused
}

// refreshMetered runs the probe off the caller's goroutine and stores the
// result. meteredUploadPaused is the only caller.
func (r *Runner) refreshMetered() {
	r.mu.Lock()
	probe := r.isMeteredFn
	r.mu.Unlock()
	if probe == nil {
		probe = netgate.IsMetered
	}
	metered, err := probe()
	if err != nil {
		slog.Debug("metered probe failed for tray status; showing uploads as paused", "err", err)
	}
	c := &r.meteredCache
	c.mu.Lock()
	c.probed = true
	c.metered = metered
	c.err = err
	c.at = time.Now()
	c.inFlight = false
	c.mu.Unlock()
}
