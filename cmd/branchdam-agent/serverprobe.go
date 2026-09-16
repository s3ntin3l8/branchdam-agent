package main

import (
	"context"
	"errors"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// serverProbeTimeout bounds one helloProbe.Probe call. Shorter than
// branchdam.DefaultTimeout (30s, client.go) -- this backs a synchronous
// button click (the Wails window's "Test connection") as well as the
// periodic background check below, and neither should be able to hang for
// half a minute.
const serverProbeTimeout = 10 * time.Second

// serverProbeInterval is how often the tray re-checks server reachability
// on its own timer, independent of the offline queue's own
// drainIntervalSecs (issue: "unknown -- never drained" persisting forever
// on a config with no offline.queueDbPath). Not yet config-file
// adjustable -- see AGENTS.md's "deliberately out of scope" note in the
// plan this shipped with.
const serverProbeInterval = 60 * time.Second

// helloProbe implements tray.ServerProbe over a real *branchdam.Client,
// the concrete wiring cmd/branchdam-agent owns -- matching
// queueCountsReader/queueDrainer/queuePruner's own relationship to their
// tray.* interfaces (internal/tray never imports internal/branchdam).
type helloProbe struct {
	client *branchdam.Client
}

// Probe calls POST /api/v1/agent/hello -- reachability + auth + version,
// with no agentId needed (unlike Handshake), which is exactly what "is the
// server reachable at all?" needs and nothing more. A bounded context is
// applied by the caller (Runner.TriggerServerProbe passes through
// whatever context it's given); this method itself layers
// serverProbeTimeout on top so neither the button-click path nor the
// periodic timer path can outlive it even if the caller passes a longer
// deadline.
func (p *helloProbe) Probe(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, serverProbeTimeout)
	defer cancel()
	resp, err := p.client.Hello(ctx)
	if err != nil {
		return "", mapProbeError(err)
	}
	if resp.Version == "" {
		// Mirrors preflight.go's own guard: a 2xx with an empty Version
		// means something is reading the wrong JSON field (hello returns
		// "version", handshake returns "serverVersion" -- see
		// HelloResponse's own doc comment), not that the server truly has
		// no version. Reporting this as reachable would hide a real
		// client/server contract mismatch behind a healthy-looking pill.
		return "", errors.New("server returned an empty version -- check for a hello/handshake field mismatch")
	}
	return resp.Version, nil
}

// mapProbeError turns a *branchdam.HTTPError's status code into a message
// an operator can act on, for the two auth failure modes config.Validate
// already warns about at config-write time (server.apiKey under 32 chars)
// and the client's own 401 case. HTTPError.Error() already omits Body
// (audit S-7 -- the body can reflect the request payload or name the
// X-API-Key), so this deliberately never reads httpErr.Body either; a
// generic status-code message is intentionally as far as this goes for
// anything other than the two curated cases below.
func mapProbeError(err error) error {
	var httpErr *branchdam.HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	switch httpErr.StatusCode {
	case 401:
		return errors.New("server rejected the agent API key")
	case 503:
		// The one 503 this client can produce a confident diagnosis for --
		// see config.go's own checkSecretPlaceholder/apiKey-length comment
		// ("under 32 characters -- the server rejects this with a 503").
		// Any other 503 cause still surfaces as this message, which is a
		// reasonable default for "the server is up but authentication
		// itself is misconfigured."
		return errors.New("server rejected the agent API key — it must be at least 32 characters")
	default:
		return err
	}
}

// registerServerProbe wires (or clears) the tray's Runner with a fresh
// helloProbe built from client, mirroring resolveServerConfig's own "dummy
// client when server isn't configured" contract: SetServerProbe(nil) is
// the honest "not configured" signal TriggerServerProbe already treats as
// ran=false, so a probe is never dialed against the
// branchdam.New("http://localhost:1", "") dummy client resolveServerConfig
// builds while server.baseUrl/apiKey/agentId are incomplete.
func registerServerProbe(runner *tray.Runner, client *branchdam.Client, configured bool) {
	if !configured {
		runner.SetServerProbe(nil)
		return
	}
	runner.SetServerProbe(&helloProbe{client: client})
}
