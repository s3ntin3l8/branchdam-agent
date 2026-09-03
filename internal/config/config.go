// Package config loads branchdam-agent's own configuration: which branchDAM
// server to talk to, the shared agent API key, this workstation's
// self-asserted agentId (see the plan's contract-gap 6 -- there is no
// per-agent identity on the server side, just a shared secret), and the
// workstation-path -> container-path map preflight prints so an operator can
// eyeball it before the first real ingest.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// StrictConfigPermissionsEnv is the environment variable that forces
// Load to refuse a config.yaml whose permission bits let group or world
// read it, regardless of the YAML value of strictConfigPermissions. It
// exists so a CI/scripted environment can enforce the same check the
// config flag does without having to edit config.yaml first -- the
// typical "I just realized this is how chmod 600" hardening step. The
// env wins over the YAML value, so a config that explicitly sets the
// flag to false can still be hard-failed in a hardened env.
const StrictConfigPermissionsEnv = "BRANCHDAM_AGENT_STRICT_CONFIG_PERMISSIONS"

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// Config is branchdam-agent's on-disk configuration
// (~/.config/branchdam-agent/config.yaml per the plan, or wherever -config
// points).
type Config struct {
	Server                  ServerConfig       `yaml:"server"`
	AgentID                 string             `yaml:"agentId"`
	PathMappings            []PathMapping      `yaml:"pathMappings"`
	Ingest                  IngestConfig       `yaml:"ingest"`
	Offline                 OfflineConfig      `yaml:"offline"`
	Prune                   PruneConfig        `yaml:"prune"`
	Tray                    TrayConfig         `yaml:"tray"`
	SelfUpdate              SelfUpdateConfig   `yaml:"selfUpdate"`
	Integrations            IntegrationsConfig `yaml:"integrations"`
	StrictConfigPermissions bool               `yaml:"strictConfigPermissions"`
}

// strictConfigPermissionsSource is the resolved source of a Config's
// effective strict-permission setting -- used by checkFilePermissions
// to phrase the strict-mode error so the remediation matches whichever
// source actually turned strict on (Hermes review on #126: a
// "set strictConfigPermissions: false" suggestion is a no-op when the
// env var is the trigger, so the message must reflect which one).
type strictConfigPermissionsSource int

const (
	strictSourceYAML strictConfigPermissionsSource = iota
	strictSourceEnv
	strictSourceEnvUnset
)

// StrictConfigPermissionsEffective reports whether the strict permission
// check is on for this Config, after applying the env-var override
// (BRANCHDAM_AGENT_STRICT_CONFIG_PERMISSIONS). The env var always wins
// over the YAML value: a config that explicitly sets the flag to false
// can still be hard-failed in a hardened CI/scripted environment, and a
// config that sets it to true stays strict even if the env is unset
// (matching the operator's explicit, file-persisted intent).
//
// An unrecognized env value (e.g. "2", "yes " with trailing space,
// "enabled" -- a typo, a leading/trailing space, or a synonym this
// helper doesn't recognize) is treated as "env says nothing" and falls
// through to the YAML value, with a one-shot slog.Warn so the operator
// learns the env var was ignored rather than silently behaving as if
// it were unset. The "env wins" contract is the value of the override,
// not its interpretation: silently turning "2" into "false" because
// the parser doesn't recognize it would re-introduce the
// silent-insecurity the env var exists to prevent (Hermes review on
// #126).
func (c Config) StrictConfigPermissionsEffective() (bool, strictConfigPermissionsSource) {
	v, ok := os.LookupEnv(StrictConfigPermissionsEnv)
	if !ok {
		return c.StrictConfigPermissions, strictSourceEnvUnset
	}
	switch {
	case v == "" || v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes") || strings.EqualFold(v, "on"):
		return true, strictSourceEnv
	case v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no") || strings.EqualFold(v, "off"):
		return false, strictSourceEnv
	default:
		slog.Warn("unrecognized value for "+StrictConfigPermissionsEnv+" -- falling back to config.strictConfigPermissions", "value", v)
		return c.StrictConfigPermissions, strictSourceEnvUnset
	}
}

// OfflineConfig configures M2's offline queue (issue #4): where queue.db
// lives and the Tier-0 container path this workstation's EVENT_NODE_CREATED
// payloads target while offline. See internal/queue's package doc and
// internal/ingest/drain.go for the state machine this backs.
type OfflineConfig struct {
	// QueueDBPath is where queue.db is created/opened. Must be writable and
	// persistent across a restart -- this is the entire crash-safety
	// contract's storage. Required for `ingest -offline` and `queue-drain`;
	// unused otherwise.
	QueueDBPath string `yaml:"queueDbPath"`
	// Tier0ContainerRoot is the server-container path prefix
	// EVENT_NODE_CREATED targets while a file's archive bytes don't exist
	// anywhere durable yet -- e.g. "/storage/staging/workstation-01" (the
	// plan's recommended per-machine subtree, since there is no per-agent
	// identity server-side to disambiguate an otherwise-shared root). The
	// branchDAM server must have a TIER0_LOCAL_STAGING storage_locations
	// entry whose rootPath resolves to (a container-side view of) this
	// prefix; see docs/offline-queue.md for what happens if it doesn't
	// (three retries, then FAILED, with the failure looking like an auth
	// problem -- see internal/branchdam's HTTPError doc comments).
	Tier0ContainerRoot string `yaml:"tier0ContainerRoot"`
	// DrainIntervalSecs is how often the tray runs an internal/ingest.Drain
	// pass on its own timer (issue #32) once a tray is running -- distinct
	// from `queue-drain -watch`'s own -interval flag, which defaults to the
	// same 5s but is a separate process's own loop. Defaults to 5 when <= 0.
	DrainIntervalSecs int `yaml:"drainIntervalSecs"`
}

// DefaultDrainIntervalSecs is OfflineConfig.DrainIntervalSecs's fallback
// when unset -- matching queue-drain -watch's own default -interval.
const DefaultDrainIntervalSecs = 5

// DrainIntervalSecsOrDefault returns o.DrainIntervalSecs, or
// DefaultDrainIntervalSecs when <= 0.
func (o OfflineConfig) DrainIntervalSecsOrDefault() int {
	if o.DrainIntervalSecs <= 0 {
		return DefaultDrainIntervalSecs
	}
	return o.DrainIntervalSecs
}

// PruneConfig configures the `prune` subcommand (branchdam#230-adjacent):
// deleting this workstation's own LocalEditRoot mirror of a file once
// POST /api/v1/agent/node-status confirms the Tier-3 archive copy is live
// and hash-verified. Only applies to offline-ingested files tracked in
// queue.db -- a plain online `ingest` leaves no durable local-path ledger,
// so it is out of scope. This is NOT real Tier-1 LOCAL_SCRATCH pruning
// (Resolve caches/proxies) -- that stays architecturally blocked; see
// branchdam's docs/workflow-coverage.md item 12.
type PruneConfig struct {
	// Enabled gates the entire subcommand -- false (the default) means
	// `prune`/`prune -watch` refuse to run at all, so accidentally invoking
	// this on a machine that hasn't opted in can't delete anything.
	Enabled bool `yaml:"enabled"`
	// MinAgeHours is the grace period after a file's own mtime (not when it
	// was queued/ingested -- deliberately mirroring branchDAM's own
	// cacheTtlHours basis) before it becomes prune-eligible, even if the
	// server already reports it verified. Defaults to 24 when Enabled is
	// true and this is left at its zero value.
	MinAgeHours int `yaml:"minAgeHours"`
	// IntervalMinutes is how often the tray runs an internal/prune.Pass on
	// its own timer (issue #32) once a tray is running, when Enabled is
	// true -- distinct from `prune -watch`'s own -interval flag, which
	// defaults to the same 30m but is a separate process's own loop.
	// Defaults to 30 when <= 0.
	IntervalMinutes int `yaml:"intervalMinutes"`
}

// DefaultPruneIntervalMinutes is PruneConfig.IntervalMinutes's fallback
// when unset -- matching `prune -watch`'s own default -interval.
const DefaultPruneIntervalMinutes = 30

// IntervalMinutesOrDefault returns p.IntervalMinutes, or
// DefaultPruneIntervalMinutes when <= 0.
func (p PruneConfig) IntervalMinutesOrDefault() int {
	if p.IntervalMinutes <= 0 {
		return DefaultPruneIntervalMinutes
	}
	return p.IntervalMinutes
}

// TrayConfig configures the M1 tray shell (issue #3): the embedded
// localhost status page and login-item registration. Both are additive to
// the headless `ingest`/`preflight`/`luminar-sync` subcommands -- nothing
// here is required for those to keep working.
type TrayConfig struct {
	// StatusAddr is the address the embedded status HTTP server binds.
	// Deliberately defaults to a loopback-only address (never a bare
	// ":port") -- the status page renders local filesystem paths and (once
	// M2 lands) queue depth, not something to expose beyond the
	// workstation itself. Defaults to "127.0.0.1:38080" when empty.
	StatusAddr string `yaml:"statusAddr"`
	// StartOnLogin registers (or removes) a per-user login item -- a
	// LaunchAgent plist on macOS, a Run-key value on Windows -- so the tray
	// starts automatically at login. Off by default; an operator opts in
	// explicitly. No effect on platforms other than windows/darwin.
	StartOnLogin bool `yaml:"startOnLogin"`
	// ConfirmDestructive gates the four destructive tray menu actions --
	// "Drain queue now", "Prune now", "Install and restart", "Roll back"
	// (issue #108 / E3 #S2-14) -- behind an explicit OK/Cancel dialog.
	// Defaults to true via defaultConfig(): destructive clicks are the
	// reason this flag exists, and an opt-OUT default would re-introduce
	// the silent-data-loss hazard the issue was filed to fix (a
	// double-click on "Prune now" with LocalEditRoot on the wrong mount
	// silently deletes files; the headless `update` command already
	// prompts by default and only bypasses with -yes). Power users who
	// want to skip the prompt set this to false explicitly.
	ConfirmDestructive bool `yaml:"confirmDestructive"`
}

// SelfUpdateConfig gates github.com/creativeprojects/go-selfupdate.
// Checking is on by default (see defaultConfig()) -- it's a read-only
// GitHub API call, never a download or a binary write. Applying an
// update found by a check is a separate, always-explicit action (a tray
// menu click, or `update`'s confirmation/-yes) that this flag does not
// by itself authorize; see CLAUDE.md's self-update invariants.
type SelfUpdateConfig struct {
	// Enabled turns self-update checks on. Default true (see
	// defaultConfig()) -- set to false explicitly for zero outbound
	// GitHub traffic.
	Enabled bool `yaml:"enabled"`
	// Repo is the "owner/name" GitHub repository slug releases are
	// published from. Defaults to "s3ntin3l8/branchdam-agent" when empty.
	Repo string `yaml:"repo"`
	// CheckIntervalHours is how often the tray re-checks for a new
	// release after its initial startup check. Defaults to 24 when zero;
	// a negative value disables re-checking (the tray still checks once
	// at startup). A tray can run for weeks, so a one-shot check would
	// never surface a release cut after startup -- unauthenticated
	// GitHub API is 60 req/hr per IP, so even an hourly check is nowhere
	// near that limit.
	CheckIntervalHours int `yaml:"checkIntervalHours"`
}

// DefaultStatusAddr is TrayConfig.StatusAddr's fallback when empty.
const DefaultStatusAddr = "127.0.0.1:38080"

// DefaultSelfUpdateRepo is SelfUpdateConfig.Repo's fallback when empty.
const DefaultSelfUpdateRepo = "s3ntin3l8/branchdam-agent"

// DefaultSelfUpdateCheckIntervalHours is SelfUpdateConfig.CheckIntervalHours's
// fallback when zero.
const DefaultSelfUpdateCheckIntervalHours = 24

// StatusAddrOrDefault returns t.StatusAddr, or DefaultStatusAddr when unset.
func (t TrayConfig) StatusAddrOrDefault() string {
	if t.StatusAddr == "" {
		return DefaultStatusAddr
	}
	return t.StatusAddr
}

// RepoOrDefault returns s.Repo, or DefaultSelfUpdateRepo when unset.
func (s SelfUpdateConfig) RepoOrDefault() string {
	if s.Repo == "" {
		return DefaultSelfUpdateRepo
	}
	return s.Repo
}

// CheckIntervalHoursOrDefault returns s.CheckIntervalHours, or
// DefaultSelfUpdateCheckIntervalHours when unset (zero).
func (s SelfUpdateConfig) CheckIntervalHoursOrDefault() int {
	if s.CheckIntervalHours == 0 {
		return DefaultSelfUpdateCheckIntervalHours
	}
	return s.CheckIntervalHours
}

// IntegrationsConfig configures branchdam-agent's third-party catalog and
// NLE integrations -- Luminar Neo today, Lightroom Classic (issue #47) and
// Apple Photos (issue #46) as future registry entries. A fresh install has
// every integration disabled and every catalog reader in dry-run mode (see
// defaultConfig()) -- nothing here contacts the server or reads a
// third-party catalog until an operator opts in explicitly.
type IntegrationsConfig struct {
	// NodeIndexPath is the JSON file mapping absolute workstation file
	// paths to the nodeUuids they were ingested as -- shared by every
	// catalog-reading integration (internal/nodeindex.Resolver), since
	// they all need to resolve the same paths back to the same node
	// graph. Matched VERBATIM by nodeindex.Resolve -- no normalization,
	// no symlink resolution (see docs/luminar-catalog.md).
	NodeIndexPath string `yaml:"nodeIndexPath"`
	// Luminar configures the Luminar Neo catalog reader (luminar-sync /
	// internal/luminar).
	Luminar CatalogSyncConfig `yaml:"luminar"`
	// Resolve carries DaVinci Resolve render-hook installer state only --
	// unlike Luminar, the render hook (hooks/resolve/) is a Python script
	// running inside Resolve's own interpreter with no config read and no
	// runtime knobs (see hooks/resolve/README.md), so there is no
	// CatalogSyncConfig-shaped entry for it.
	Resolve ResolveHookConfig `yaml:"resolve"`
}

// CatalogSyncConfig is the uniform shape every catalog-reader integration
// uses -- Luminar today, lrcat (issue #47) and applephotos (issue #46)
// next. A new integration adds a field of this type to IntegrationsConfig,
// not a new struct shape.
//
// Deliberately does NOT carry queryFile/derivativeSuffixes-equivalent
// knobs, even though internal/luminar.Syncer and luminar-sync itself
// support overriding both: those two stay CLI-only correction levers (see
// docs/luminar-catalog.md), by design. A tray-driven, config-persisted
// override would convert a supervised, one-shot CLI override into an
// automatic rule that runs on syncIntervalMinutes's timer, and branchDAM's
// PostEdgeAttached is idempotent but "never refreshes confidence or
// evidence" (internal/branchdam/events.go) -- a wrong edge a mistuned
// override emits is then permanently wrong, fixable only by a server-side
// data-correction migration. It would also silently falsify
// luminar.SchemaMappingVersion's provenance claim about which query/suffix
// set an edge's evidence was produced by.
type CatalogSyncConfig struct {
	// Enabled gates whether the tray registers a syncer for this
	// integration at all -- false (the default) means it never runs,
	// on a timer or via "Sync now". Deliberately NOT cross-validated
	// against CatalogPath in Validate(): an operator ticking "Enabled"
	// before setting a catalog path is a normal, transient state, not a
	// config error -- see cmd/branchdam-agent's syncer-registration gate,
	// which is where "enabled but not yet configured" is actually
	// handled (a nil syncer, never a Validate problem).
	Enabled bool `yaml:"enabled"`
	// CatalogPath is the third-party application's catalog file this
	// integration reads read-only. Must not contain '?' or '#' -- see
	// Validate().
	CatalogPath string `yaml:"catalogPath"`
	// DryRun, when true, resolves and logs what a sync pass would emit
	// without contacting the server at all. Defaults to true (see
	// defaultConfig()) -- an operator turns this off explicitly, per
	// integration, once they've read the dry-run log. This is the same
	// "deliberately non-zero default" shape as SelfUpdateConfig.Enabled;
	// never construct a CatalogSyncConfig literal and read this field --
	// always go through Load.
	DryRun bool `yaml:"dryRun"`
	// SyncIntervalMinutes is how often the tray runs a sync pass on its
	// own timer once this integration is enabled. Zero means the
	// default (DefaultSyncIntervalMinutes); a NEGATIVE value means
	// "manual only" -- the tray never syncs on its own, though "Sync
	// now" still works. This mirrors SelfUpdateConfig.CheckIntervalHours's
	// 0-means-default/negative-means-never convention, not
	// OfflineConfig.DrainIntervalSecs's "<= 0 means default": unlike a
	// queue drain, "enabled, but I only ever sync by hand" is a real,
	// deliberate mode here -- a pass reads a catalog file the operator
	// may have open in the third-party application right now.
	// Deliberately NOT checked for negative values in Validate() (contrast
	// TimeoutSecs below) precisely because negative is meaningful.
	SyncIntervalMinutes int `yaml:"syncIntervalMinutes"`
	// TimeoutSecs bounds one sync pass. Defaults to
	// DefaultIntegrationTimeoutSecs when <= 0 -- matching luminar-sync's
	// own -timeout flag default.
	TimeoutSecs int `yaml:"timeoutSecs"`
}

// ResolveHookConfig configures the DaVinci Resolve render-hook installer
// (internal/resolvehook).
type ResolveHookConfig struct {
	// ScriptsDir overrides the auto-detected candidate list of Resolve
	// Scripts/Utility directories. Empty (the default) means "use the
	// per-OS candidate list" -- see internal/resolvehook.CandidateDirs.
	ScriptsDir string `yaml:"scriptsDir"`
}

// DefaultSyncIntervalMinutes is CatalogSyncConfig.SyncIntervalMinutes's
// fallback when zero -- an hour, deliberately far coarser than the drain
// (5s) and prune (30m) timers: a catalog sync reads a third-party
// application's live database and re-POSTs edges the server treats as
// idempotent no-ops (see CatalogSyncConfig's own doc comment), so there is
// nothing to gain from a tight cadence.
const DefaultSyncIntervalMinutes = 60

// DefaultIntegrationTimeoutSecs is CatalogSyncConfig.TimeoutSecs's fallback
// when <= 0 -- matches luminar-sync's own -timeout flag default.
const DefaultIntegrationTimeoutSecs = 30

// SyncIntervalMinutesOrDefault returns c.SyncIntervalMinutes, or
// DefaultSyncIntervalMinutes when zero. A negative value is returned
// verbatim and means "manual only" -- see the field's own doc comment.
func (c CatalogSyncConfig) SyncIntervalMinutesOrDefault() int {
	if c.SyncIntervalMinutes == 0 {
		return DefaultSyncIntervalMinutes
	}
	return c.SyncIntervalMinutes
}

// TimeoutSecsOrDefault returns c.TimeoutSecs, or
// DefaultIntegrationTimeoutSecs when <= 0.
func (c CatalogSyncConfig) TimeoutSecsOrDefault() int {
	if c.TimeoutSecs <= 0 {
		return DefaultIntegrationTimeoutSecs
	}
	return c.TimeoutSecs
}

// IngestConfig configures M1's SD-card ingest core: where the two copies
// land and what relative-path template both derive from, and card-detection
// polling. ArchiveRoot/LocalEditRoot are workstation-native paths (this
// process writes to them with plain os.* calls, never through branchDAM's
// storage.Guard -- the agent runs on the workstation, not the server).
// ArchiveRoot's workstation-to-container translation for the
// EVENT_NODE_CREATED payload goes through PathMappings above; LocalEditRoot
// is never translated or sent to the server at all.
type IngestConfig struct {
	// ArchiveRoot is the workstation path backing the Tier-3 archive
	// destination (e.g. a mounted NAS share). Must have a matching
	// PathMapping so the agent can translate a written file's path into the
	// server-container, absolute, symlink-free form NodeCreatedPayload.FilePath
	// requires.
	ArchiveRoot string `yaml:"archiveRoot"`
	// LocalEditRoot is the workstation path for the local edit copy (fast
	// local/NVMe scratch). Never sent to the server -- the local copy is not
	// a server-tracked node.
	LocalEditRoot string `yaml:"localEditRoot"`
	// PathTemplate is the relative-path template both ArchiveRoot and
	// LocalEditRoot derive a destination path from, so the local copy
	// mirrors the archive subtree by construction. Supports
	// {yyyy}/{mm}/{dd}/{camera_model}/{original_name} placeholders -- see
	// internal/ingest's naming.go. Defaults to
	// "{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}" when empty.
	PathTemplate string `yaml:"pathTemplate"`
	// CardRoots are the parent directories polled for newly mounted
	// removable volumes (e.g. "/media/$USER", "/run/media/$USER" on Linux,
	// "/Volumes" on macOS). Only used by the `ingest --watch` poll loop, not
	// by a direct `ingest --card <path>` invocation.
	CardRoots []string `yaml:"cardRoots"`
	// PollIntervalSecs is the card-detection poll interval; defaults to 2
	// (matching the plan's "poll every ~2s is sufficient") when <= 0.
	PollIntervalSecs int `yaml:"pollIntervalSecs"`
	// RequireUnbuffered makes a fallback to VerifyMethodBufferedFloor fatal
	// during verification (withholding safe-eject and failing the ingest run)
	// rather than advisory/logged. Defaults to false.
	RequireUnbuffered bool `yaml:"requireUnbuffered"`
	// RequireDCIM skips volumes that do not contain a DCIM/ subdirectory.
	// Default false. When true, only camera cards with a standard DCIM layout
	// are auto-detected; USB sticks and backup drives are silently skipped.
	RequireDCIM bool `yaml:"requireDCIM"`
	// UploadStream enables direct HTTP streaming upload to POST /api/v1/agent/upload
	// rather than writing directly to a mounted ArchiveRoot. Defaults to false.
	UploadStream bool `yaml:"uploadStream"`
	// AllowedExtensions is the M5/#100 allowlist of file extensions a card
	// walk ingests; the walk also unconditionally skips OS-metadata files
	// (Thumbs.db, System Volume Information, any dotfile) regardless of
	// what's here, but a non-empty AllowedExtensions narrows the set
	// further. When empty (the default), the walk accepts every file the
	// OS-metadata skip does not rule out -- preserving the pre-#100
	// behavior of every existing config.
	//
	// Matching is case-insensitive: an operator can write "JPG" or "jpg"
	// or "Jpg" in YAML, and a file's "IMG_0001.JPG" still matches.
	// Extensions are compared without the leading dot ("jpg" in YAML
	// matches "IMG_0001.jpg" on disk).
	AllowedExtensions []string `yaml:"allowedExtensions"`
	// AutoImportPaths is the allowlist of volume paths or labels that bypass
	// the card-detection confirmation dialog (issue #79). When a newly mounted
	// volume matches an entry here, it is ingested immediately without prompting.
	AutoImportPaths []string `yaml:"autoImportPaths"`
}

// ServerConfig is the branchDAM server this agent reports to.
type ServerConfig struct {
	BaseURL string `yaml:"baseUrl"`
	// APIKey is the shared secret sent as the raw X-API-Key header (no
	// scheme). The server rejects one under 32 chars with a 503 plain-text
	// body ("agent authentication is not configured") -- see
	// internal/branchdam's conformance contract doc.
	APIKey string `yaml:"apiKey"`
}

// PathMapping is one workstation-path -> container-path rewrite rule.
// branchDAM's own agent event payloads must carry server-container, absolute,
// symlink-free paths -- there is no server-side rewrite pass on agent
// payloads (unlike cfg.PathRewrites, whose only consumer is the project-file
// resolver) -- so the agent must translate a workstation path itself before
// it ever appears in a request body. preflight prints the resolved map so an
// operator can confirm it before the first real ingest.
type PathMapping struct {
	WorkstationPath string `yaml:"workstationPath"`
	ContainerPath   string `yaml:"containerPath"`
}

func defaultConfig() Config {
	return Config{
		Server: ServerConfig{
			BaseURL: "http://localhost:8080",
		},
		// Checking for an update is on by default -- it's a read-only
		// GitHub API call, never a download or a binary write, and an
		// operator who never learns a release exists can't act on it.
		// Applying one is a separate, always-explicit decision (a tray
		// menu click, or `update`'s confirmation prompt / -yes) that
		// this flag does not by itself authorize -- see
		// internal/selfupdate's doc comment and CLAUDE.md's self-update
		// invariants. An operator who wants zero outbound GitHub traffic
		// sets selfUpdate.enabled: false explicitly.
		SelfUpdate: SelfUpdateConfig{Enabled: true},
		// Dry run ON by default, including when config.yaml has no
		// integrations: block at all -- Load unmarshals over this
		// default, so an explicit dryRun: false in config still wins.
		// A fresh install resolves and logs what a sync WOULD emit and
		// contacts no server; an operator turns this off explicitly,
		// per integration, once they've read the dry-run log. See
		// CatalogSyncConfig.DryRun's own doc comment.
		Integrations: IntegrationsConfig{
			Luminar: CatalogSyncConfig{DryRun: true},
		},
		// ConfirmDestructive ON by default (issue #108 / E3 #S2-14): a
		// destructive click -- "Prune now" against the wrong mount, a
		// self-update restart mid-render -- is the very reason the
		// field exists, and an opt-OUT default would re-introduce the
		// silent-data-loss hazard the issue was filed to fix. Mirrors
		// SelfUpdate.Enabled's own "on by default" pattern, same
		// "explicit value still wins" guarantee.
		Tray: TrayConfig{ConfirmDestructive: true},
	}
}

// Load reads path, expands ${VAR} environment references, and parses it as
// YAML into Config, applying defaultConfig()'s zero-value defaults first.
//
// After a successful parse, Load also checks the file's permission bits
// (issue #97 / audit S-5): if path's group-or-world read bits are set
// and the parsed config carries a real server.apiKey, Load logs a
// slog.Warn naming the file and the recommended `chmod 600`. The
// purpose is to surface the silent insecurity of a hand-edited
// config.yaml left at the umask default (mode 0o644) -- Patch already
// writes mode 0o600, so the gap is only on hand-edited files. When
// strict mode is on (config.strictConfigPermissions or the
// BRANCHDAM_AGENT_STRICT_CONFIG_PERMISSIONS env var), the same check
// becomes a hard error and Load returns it instead of warning.
func Load(path string) (Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	expanded := expandEnv(string(data))

	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	if err := checkFilePermissions(path, cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// checkFilePermissions is the post-Load security gate on config.yaml's
// filesystem mode (issue #97 / audit S-5). It runs only when the parsed
// config carries a real server.apiKey -- the only secret the file holds
// today -- and only the config file itself is checked, not its parent
// directory. A 0o077 mask catches any of the nine group+world bits
// (group read/write/exec + world read/write/exec), so 0o640 (group
// readable) trips the same warning as 0o644 (world readable).
//
// Behavior:
//   - Permissions clean (mask == 0): no-op.
//   - Permissions loose AND no apiKey: no-op (the file carries no
//     secret to protect; warning would be noise).
//   - Permissions loose AND apiKey AND !strict: slog.Warn with the
//     path so an operator can find it. The Load call still succeeds --
//     a warning, not an error, so an existing deployment that just
//     noticed the gap keeps starting while the operator fixes it.
//   - Permissions loose AND apiKey AND strict: return a hard error
//     naming the path; the caller's start-up gate surfaces it.
//
// Platform notes:
//   - Windows: os.FileMode.Perm() returns 0o666 for any non-readonly
//     file (Windows has no POSIX mode bits; ACLs govern access), so
//     the 0o077 mask would ALWAYS trip on Windows and the strict path
//     would refuse every config with an apiKey, regardless of the
//     actual ACL. The check is a no-op on Windows by design -- the
//     same audit (S-5) recommends operator-driven ACL review on
//     Windows (Explorer > Properties > Security), which is outside
//     this package's scope. issue #97's acceptance criterion is
//     silently-insecure 0o644 hand-edited files; that case is
//     unique to POSIX umasks and cannot arise on Windows in the same
//     way (Hermes review on #126).
//   - POSIX: a Stat that fails silently skips the check rather than
//     failing Load -- a successful ReadFile moments earlier proved the
//     file exists; a missing permission check is the lesser evil vs.
//     failing a config load the operator knows is readable.
func checkFilePermissions(path string, cfg Config) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if cfg.Server.APIKey == "" {
		return nil
	}

	strict, source := cfg.StrictConfigPermissionsEffective()
	if strict {
		// The remediation depends on which source actually turned
		// strict on. The env wins over the YAML value, so suggesting
		// "set strictConfigPermissions: false" when the env is the
		// trigger is a no-op -- a Hermes review finding on #126.
		var fix string
		switch source {
		case strictSourceEnv:
			fix = fmt.Sprintf("unset %s (or set it to 0/false) to warn instead", StrictConfigPermissionsEnv)
		default:
			fix = fmt.Sprintf("set strictConfigPermissions: false (or unset %s) to warn instead", StrictConfigPermissionsEnv)
		}
		return fmt.Errorf("config file %s is group/world readable (mode %#o) and contains a server.apiKey; chmod 600 and re-run (strict mode -- %s)", path, fi.Mode().Perm(), fix)
	}
	slog.Warn("config.yaml is group/world-readable; consider 'chmod 600'", "path", path, "mode", fmt.Sprintf("%#o", fi.Mode().Perm()))
	return nil
}

// DefaultPath returns the standard per-user location for config.yaml --
// os.UserConfigDir()'s branchdam-agent subdirectory
// (~/.config/branchdam-agent/config.yaml on Linux,
// %AppData%\branchdam-agent\config.yaml on Windows,
// ~/Library/Application Support/branchdam-agent/config.yaml on macOS).
// Neither DefaultPath nor Load creates this file or its parent directory --
// a first-run bootstrap command that does is planned as follow-on work
// (issue #30), not part of this package.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve default config directory: %w", err)
	}
	return filepath.Join(dir, "branchdam-agent", "config.yaml"), nil
}

// ResolvePath implements every subcommand's -config flag semantics: an
// explicit non-empty flagValue is used as-is (back-compatible with every
// existing invocation and script that already passes -config). An empty
// flagValue -- the new default -- means "./config.yaml if one exists in
// the current directory, else DefaultPath()," so a config.yaml dropped
// next to the binary still wins (matching this project's original
// behavior) while an operator who has never passed -config still resolves
// to a real, documented location instead of a hardcoded "config.yaml" that
// silently depends on the process's current working directory.
func ResolvePath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml", nil
	}
	return DefaultPath()
}

// Problem is one thing Validate found wrong, or suspicious, about a
// Config -- reported as data, not a fatal error, so a caller like
// `preflight` can print every problem at once, and each subcommand
// decides for itself which of these are blocking for what it's about to
// do (a bare `luminar-sync` run doesn't care that ingest.archiveRoot is
// empty; `tray` does).
//
// Severity is advisory metadata: ZeroValueProblem (the empty string)
// means "structural failure" -- the value is wrong in any context, and
// callers like preflight treat it as blocking by default. SeverityWarning
// marks a value that is suspicious-but-tolerable (e.g. cleartext http on
// a loopback host -- legitimate for a co-located dev server) and callers
// surface it but do not block on it.
//
// "Blocking or not?" is decided by Problem.Advisory(), not by comparing
// Severity == SeverityWarning at every call site -- so adding a future
// SeverityInfo / SeverityHint tier only needs to extend Advisory(), not
// every reader.
type Problem struct {
	Field    string
	Message  string
	Severity string
}

const (
	// SeverityWarning marks a Problem as advisory: surfaced to the operator
	// but not blocking. Used today by checkServerBaseURL for cleartext
	// http on a loopback host (issue #96) -- a legitimate local-dev
	// posture that no operator should have to scrub through a hard
	// failure to use.
	SeverityWarning = "warning"
)

// String renders p as "field: message", the form preflight and the tray's
// startup-error surface both print.
func (p Problem) String() string {
	return fmt.Sprintf("%s: %s", p.Field, p.Message)
}

// Advisory reports whether p should block a settings-driven config
// mutation or a process's startup gate. Today only SeverityWarning is
// advisory; the zero-value Severity is the structural-failure default.
//
// Centralizing the "blocking or not?" decision here means call sites in
// cmd/branchdam-agent (settings.firstBlockingProblem, preflight, the tray
// startup gate) don't each compare Severity with `== warning` / `!= warning`
// and silently disagree when a future SeverityInfo / SeverityHint tier
// lands -- a Hermes review finding on the PR that introduced Severity.
func (p Problem) Advisory() bool {
	return p.Severity == SeverityWarning
}

// Validate runs the checks that apply regardless of which subcommand is
// running -- values that are never correct in any context -- as opposed
// to "this subcommand additionally requires X to be set," which stays
// each subcommand's own concern (see cmd/branchdam-agent's per-subcommand
// requiredness checks). Its main job is catching the silent footgun
// expandEnv's own doc comment warns about: Load leaves an unset ${VAR} as
// the literal placeholder string, which then passes a plain `!= ""` check
// and fails downstream in a way that looks like a server misconfiguration
// (a 503 from a too-short apiKey) rather than a local one.
func (c Config) Validate() []Problem {
	var problems []Problem

	// checkPlaceholder's message names the matched placeholder (e.g.
	// "${BRANCHDAM_AGENT_API_KEY}") -- genuinely useful for an operator
	// scanning several fields at once, and safe for every field here
	// except server.apiKey, which goes through checkSecretPlaceholder
	// instead: a *structurally separate* function, not a boolean flag on
	// this one. A shared function with a redact-after-the-fact branch
	// still has a code path where the matched substring is interpolated
	// into a string before being overwritten -- CodeQL's
	// go/clear-text-logging (a taint tracker, not a value-flow-sensitive
	// one) flagged exactly that path once Validate()'s problems started
	// reaching slog (tray.go), not just fmt.Fprintln (preflight.go, a
	// sink that query doesn't cover), even though the overwrite always
	// wins at runtime. checkSecretPlaceholder never extracts or
	// interpolates any substring of value at all -- only a bool
	// (envVarRe.MatchString) crosses from the sensitive field into the
	// message -- so there is no data-flow path left for the tool to
	// follow, not just one that happens to be dead at runtime.
	checkPlaceholder := func(field, value string) {
		if m := envVarRe.FindString(value); m != "" {
			problems = append(problems, Problem{
				Field:   field,
				Message: fmt.Sprintf("still contains the unexpanded placeholder %s -- the environment variable was not set when this config was loaded", m),
			})
		}
	}

	checkSecretPlaceholder := func(field, value string) {
		if envVarRe.MatchString(value) {
			problems = append(problems, Problem{
				Field:   field,
				Message: "still contains an unexpanded placeholder (value withheld) -- the environment variable was not set when this config was loaded",
			})
		}
	}

	checkPlaceholder("server.baseUrl", c.Server.BaseURL)
	checkSecretPlaceholder("server.apiKey", c.Server.APIKey)
	checkPlaceholder("agentId", c.AgentID)
	checkPlaceholder("ingest.archiveRoot", c.Ingest.ArchiveRoot)
	checkPlaceholder("ingest.localEditRoot", c.Ingest.LocalEditRoot)
	checkPlaceholder("ingest.pathTemplate", c.Ingest.PathTemplate)
	for i, root := range c.Ingest.CardRoots {
		checkPlaceholder(fmt.Sprintf("ingest.cardRoots[%d]", i), root)
	}
	for i, path := range c.Ingest.AutoImportPaths {
		checkPlaceholder(fmt.Sprintf("ingest.autoImportPaths[%d]", i), path)
	}
	checkPlaceholder("offline.queueDbPath", c.Offline.QueueDBPath)
	checkPlaceholder("offline.tier0ContainerRoot", c.Offline.Tier0ContainerRoot)
	checkPlaceholder("tray.statusAddr", c.Tray.StatusAddr)
	checkPlaceholder("selfUpdate.repo", c.SelfUpdate.Repo)
	checkPlaceholder("integrations.nodeIndexPath", c.Integrations.NodeIndexPath)
	checkPlaceholder("integrations.luminar.catalogPath", c.Integrations.Luminar.CatalogPath)
	checkPlaceholder("integrations.resolve.scriptsDir", c.Integrations.Resolve.ScriptsDir)
	for i, m := range c.PathMappings {
		checkPlaceholder(fmt.Sprintf("pathMappings[%d].workstationPath", i), m.WorkstationPath)
		checkPlaceholder(fmt.Sprintf("pathMappings[%d].containerPath", i), m.ContainerPath)
	}

	if c.Server.APIKey != "" && !envVarRe.MatchString(c.Server.APIKey) && len(c.Server.APIKey) < 32 {
		problems = append(problems, Problem{
			Field:   "server.apiKey",
			Message: `under 32 characters -- the server rejects this with a 503 ("agent authentication is not configured")`,
		})
	}

	// Server.BaseURL is concatenated verbatim with "/api/v1/agent/..." in
	// internal/branchdam/client.go's post() -- so a trailing slash here
	// becomes "host//api/...", and an "http://" on a non-loopback host is
	// a cleartext wire exposure that no operator should land on by typo.
	// The placeholder check above runs first; if it flagged BaseURL, skip
	// these structural checks so the operator sees the placeholder
	// message rather than a downstream parse error against "${...}".
	if !envVarRe.MatchString(c.Server.BaseURL) {
		problems = append(problems, checkServerBaseURL(c.Server.BaseURL)...)
	}

	if c.Ingest.PollIntervalSecs < 0 {
		problems = append(problems, Problem{Field: "ingest.pollIntervalSecs", Message: "must not be negative"})
	}
	if c.Prune.MinAgeHours < 0 {
		problems = append(problems, Problem{Field: "prune.minAgeHours", Message: "must not be negative"})
	}
	if c.Offline.DrainIntervalSecs < 0 {
		problems = append(problems, Problem{Field: "offline.drainIntervalSecs", Message: "must not be negative"})
	}
	if c.Prune.IntervalMinutes < 0 {
		problems = append(problems, Problem{Field: "prune.intervalMinutes", Message: "must not be negative"})
	}
	problems = append(problems, checkCatalogSync("integrations.luminar", c.Integrations.Luminar)...)
	// When lrcat (#47) / applephotos (#46) land, their CatalogSyncConfig
	// fields go through this SAME checkCatalogSync call (one per
	// integration, field-prefixed by its own key) -- not a hand-copied
	// pair of checks -- so a future integration can't silently ship
	// without the '?'/'#' catalog-path safety net or the timeoutSecs
	// sanity check.

	return problems
}

// checkServerBaseURL enforces the BaseURL invariants an operator can
// silently get wrong: a typo, a missing scheme, a trailing slash, or an
// http:// host on a public network. internal/branchdam.Client.post()
// concatenates baseURL + "/api/v1/agent/..." verbatim, so a trailing slash
// produces "host//api/v1/agent/..." (rejected by most servers as a path
// normalization), and a cleartext transport on a non-loopback host is a
// wire-exposure the agent must refuse to use rather than send the
// X-API-Key shared secret over.
//
// Kept as its own function (mirroring checkCatalogSync's shape) so the
// base URL policy can evolve without growing Validate()'s own block --
// e.g. a future "block private-RFC1918 hosts without an opt-in" rule
// adds one more branch here, not another inline stanza in Validate().
func checkServerBaseURL(raw string) []Problem {
	var problems []Problem

	// Defensive: an empty BaseURL has already been flagged as a
	// placeholder by checkPlaceholder above; this function is reached
	// only when that check didn't fire, so a literal "" at this point
	// is a zero-value Config (not a config-file typo) and is not
	// something an operator needs to be told about at Validate time --
	// defaultConfig() fills it in for the file path, and the
	// requiredness-for-each-subcommand checks live in
	// cmd/branchdam-agent, not here.
	if raw == "" {
		return nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		problems = append(problems, Problem{
			Field:   "server.baseUrl",
			Message: fmt.Sprintf("not a valid URL: %v", err),
		})
		return problems
	}

	// Must be absolute (have a scheme). A relative path like
	// "/api/v1/agent/hello" or "branchdam.example.com" parses
	// successfully but with no scheme -- which would let the HTTP client
	// silently build a request to the wrong host.
	if !u.IsAbs() || u.Scheme == "" {
		problems = append(problems, Problem{
			Field:   "server.baseUrl",
			Message: fmt.Sprintf("must be an absolute URL with an http or https scheme (got %q)", raw),
		})
		return problems
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		problems = append(problems, Problem{
			Field:   "server.baseUrl",
			Message: fmt.Sprintf("scheme must be http or https (got %q)", u.Scheme),
		})
		return problems
	}

	// Trailing slash on the path would concatenate to "host//api/..."
	// in client.go's baseURL+path. Path is empty for a bare host
	// ("http://example.com"), in which case Path == "" is fine.
	if strings.HasSuffix(u.Path, "/") {
		problems = append(problems, Problem{
			Field:   "server.baseUrl",
			Message: "must not end with a trailing slash -- internal/branchdam/client.go concatenates this URL with \"/api/v1/...\" verbatim, producing a double-slash path",
		})
	}

	// Cleartext http on a non-loopback host is refused outright. A
	// shared X-API-Key is never worth sending over the wire in cleartext
	// to anything but localhost; loopback http stays a warning, not a
	// refusal, so a workstation pointing at a co-located dev server
	// keeps working without an operator having to reach for TLS first.
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		problems = append(problems, Problem{
			Field:   "server.baseUrl",
			Message: "uses cleartext http on a non-loopback host -- the X-API-Key shared secret would be sent in cleartext; use https or a loopback host",
		})
		return problems
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		problems = append(problems, Problem{
			Field:    "server.baseUrl",
			Severity: SeverityWarning,
			Message:  "uses cleartext http on a loopback host -- fine for a local dev server, but use https for anything reachable beyond this workstation",
		})
	}

	return problems
}

// isLoopbackHost reports whether h is one of the loopback host names an
// operator is reasonably likely to type: 127.0.0.1, ::1 (any zone), and
// "localhost". Hostnames are compared case-insensitively. net.ParseIP
// handles bracketed-IPv6 ("::1") the same way url.URL.Hostname() strips
// the brackets, so no extra unwrapping is needed here.
func isLoopbackHost(h string) bool {
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// checkCatalogSync runs the field-agnostic checks every CatalogSyncConfig
// needs, regardless of which integration it belongs to -- prefix is the
// integration's own dotted key (e.g. "integrations.luminar"). Kept as one
// shared function specifically so adding lrcat/applephotos means one call
// site here, not two more hand-copied blocks that can independently drift
// (a Hermes review finding on the PR that introduced CatalogSyncConfig).
//
// Deliberately does NOT check SyncIntervalMinutes for a negative value --
// unlike every other interval field in this package, negative is
// meaningful here ("manual only"; see CatalogSyncConfig.SyncIntervalMinutes's
// own doc comment), not a mistake.
func checkCatalogSync(prefix string, c CatalogSyncConfig) []Problem {
	var problems []Problem

	if c.TimeoutSecs < 0 {
		problems = append(problems, Problem{Field: prefix + ".timeoutSecs", Message: "must not be negative"})
	}

	// internal/luminar.Open (and any future catalog-reader integration
	// following the same convention) concatenates CatalogPath into a
	// "file:<path>?mode=ro" SQLite URI and rejects '?'/'#' outright: a '?'
	// could inject a second query parameter and silently open the catalog
	// ?immutable=1, the one mode that package exists to never use against a
	// live-WAL catalog. Checked here, inline, rather than by importing
	// internal/luminar -- internal/config must stay dependency-free -- so a
	// runtime-only failure becomes a config problem an operator sees once.
	if strings.ContainsAny(c.CatalogPath, "?#") {
		problems = append(problems, Problem{
			Field:   prefix + ".catalogPath",
			Message: "must not contain '?' or '#' -- the path is concatenated into a SQLite file: URI and would be misread as query parameters",
		})
	}

	return problems
}

// expandEnv replaces every ${VAR} with the environment variable's value,
// leaving the placeholder untouched if VAR is unset. It does NOT support a
// ":-default" separator -- everything between "${" and "}" is treated as one
// literal environment-variable name, so "${VAR:-default}" becomes the
// literal string "${VAR:-default}" whenever VAR is unset, not "default".
// Carried over from the go-http-template this repo was scaffolded from,
// where getting this wrong was a real bug once -- see the template's
// CLAUDE.md invariant of the same name.
func expandEnv(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		key := strings.TrimSpace(envVarRe.FindStringSubmatch(match)[1])
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return match
	})
}
