package tray

import (
	"context"
	"encoding/json"
	"time"
)

// ServerProbe is the subset of *branchdam.Client's behavior
// Runner.TriggerServerProbe needs for an on-demand "is the server
// reachable?" check -- an interface, like Drainer/Pruner/Ingester, so
// internal/tray stays unit-testable with a fake and never needs to import
// internal/branchdam itself. Deliberately independent of Drainer: a drain
// pass only exists when offline.queueDbPath is configured (see
// QueueStatus.Configured's own doc comment), but confirming the agent can
// reach and authenticate against the server is meaningful with no offline
// queue at all -- that's the whole reason this exists as its own seam
// rather than reusing DrainSummary.HandshakeOK/LastHandshakeAt.
type ServerProbe interface {
	Probe(ctx context.Context) (version string, err error)
}

// ProbeResult is a condensed view of one ServerProbe.Probe call -- the
// tray-local mirror of internal/branchdam.HelloResponse, same reasoning as
// DrainSummary mirroring internal/ingest.DrainStats. At is set by
// Runner.TriggerServerProbe, not by the ServerProbe implementation, so it
// always reflects when the tray actually ran the check.
type ProbeResult struct {
	OK      bool      `json:"ok"`
	At      time.Time `json:"at"`
	Version string    `json:"version,omitempty"`
	Err     error     `json:"-"`
}

// MarshalJSON renders Err as a string -- see IngestSummary.MarshalJSON's
// doc comment (tray.go) for why and how. Err is a fresh field on a fresh
// type, so unlike DrainSummary/QueueStatus (which predate the lowercase
// json-tag convention and are read back capitalized by app.js) this uses
// IngestSummary's own "err", lowercase, convention throughout.
func (r ProbeResult) MarshalJSON() ([]byte, error) {
	type alias ProbeResult
	return json.Marshal(struct {
		alias
		Err string `json:"err,omitempty"`
	}{alias: alias(r), Err: errString(r.Err)})
}
