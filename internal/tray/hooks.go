package tray

import (
	"context"
	"time"
)

// HookID names one installable script hook -- DaVinci Resolve's render
// hook (issue #60) is the only entry today. A string, like IntegrationID,
// though deliberately a SEPARATE type: a hook is not a catalog-sync
// integration (no CatalogSyncConfig, no IntegrationSyncer), so it does not
// belong in Integrations()'s registry -- see that function's own doc
// comment.
type HookID string

// HookResolve is DaVinci Resolve's render-hook installer.
const HookResolve HookID = "resolve"

// Hooks is the compile-time registry every installable hook is listed in
// -- mirrors Integrations()'s own shape (a function, not an exported
// package var) for consistency, even though there is exactly one entry
// today and no second hook is currently anticipated: it costs nothing to
// keep Status()'s construction symmetric with Integrations'.
func Hooks() []HookID {
	return []HookID{HookResolve}
}

// HookDescriptor is one compile-time registry entry for the hooks menu
// (issue #68) -- the HookID/Title counterpart to IntegrationDescriptor. A
// separate function from Hooks() (which only returns IDs and already has
// callers -- Status()'s own construction, and a Runner test -- that don't
// need a title) rather than changing Hooks()'s own signature.
type HookDescriptor struct {
	ID    HookID
	Title string
}

// HookDescriptors is the compile-time registry newHooksMenu builds items
// from -- mirrors Integrations()'s own doc-comment rationale exactly: the
// menu is built once with no teardown-and-rebuild path (onReady), so this
// list must stay static.
func HookDescriptors() []HookDescriptor {
	return []HookDescriptor{
		{ID: HookResolve, Title: "DaVinci Resolve"},
	}
}

// HookState is Runner's cached view of one hook's on-disk state -- NEVER
// computed live from Status() or the menu-refresh tick (see
// Runner.SetHookState's own doc comment for why: a filesystem stat plus a
// checksum read on a possibly-networked scriptsDir would reproduce the
// exact hazard statusQueueReadTimeout was added to fix). At is set by
// Runner (SetHookState or TriggerHookInstall), not by the implementation,
// same discipline as SyncSummary.At.
//
// Installed vs. UpToDate vs. "modified" are indistinguishable by design --
// both a hand-edited copy and an older shipped version are the same
// checksum mismatch against the embedded source's own hash. A caller
// renders that as "installed but modified or out of date" rather than
// guessing which.
type HookState struct {
	At        time.Time
	Dir       string // "" only when no candidate Scripts folder exists at all
	Path      string // Dir/<file name>, set when Installed
	Installed bool
	UpToDate  bool
	Err       error
}

// HookInstaller is the KindHookInstall-shaped counterpart to
// IntegrationSyncer (see that type's own doc comment) -- an interface, like
// Drainer/Pruner/IntegrationSyncer, so this package stays free of
// internal/resolvehook and unit-testable with a fake.
type HookInstaller interface {
	// Install writes (or overwrites) the hook and returns the resulting
	// state. Never prompts -- an implementation with no candidate
	// directory returns an error telling the operator to pick one (via
	// integrations.resolve.scriptsDir).
	Install(ctx context.Context) (HookState, error)
	// Reveal opens the Scripts folder (or its nearest existing ancestor,
	// if the folder itself doesn't exist yet) in the OS file manager.
	Reveal() error
}

// HookStatus is Runner's RUNTIME view of one hook, paired with a (later
// PR's) menu -- mirrors IntegrationStatus's own Registered/nil-state
// contract: Registered=false means cmd/branchdam-agent did not wire an
// installer for this ID, never an error.
type HookStatus struct {
	ID         HookID
	Registered bool
	State      *HookState
}

// Hook looks up st's entry for id by ID, mirroring
// Status.Integration/SettingsView.Integration's own by-ID-not-index
// lookup convention throughout this package.
func (st Status) Hook(id HookID) (HookStatus, bool) {
	for _, hs := range st.Hooks {
		if hs.ID == id {
			return hs, true
		}
	}
	return HookStatus{}, false
}
