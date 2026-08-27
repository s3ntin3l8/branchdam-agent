// Package config loads branchdam-agent's own configuration: which branchDAM
// server to talk to, the shared agent API key, this workstation's
// self-asserted agentId (see the plan's contract-gap 6 -- there is no
// per-agent identity on the server side, just a shared secret), and the
// workstation-path -> container-path map preflight prints so an operator can
// eyeball it before the first real ingest.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// Config is branchdam-agent's on-disk configuration
// (~/.config/branchdam-agent/config.yaml per the plan, or wherever -config
// points).
type Config struct {
	Server       ServerConfig     `yaml:"server"`
	AgentID      string           `yaml:"agentId"`
	PathMappings []PathMapping    `yaml:"pathMappings"`
	Ingest       IngestConfig     `yaml:"ingest"`
	Offline      OfflineConfig    `yaml:"offline"`
	Prune        PruneConfig      `yaml:"prune"`
	Tray         TrayConfig       `yaml:"tray"`
	SelfUpdate   SelfUpdateConfig `yaml:"selfUpdate"`
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
	}
}

// Load reads path, expands ${VAR} environment references, and parses it as
// YAML into Config, applying defaultConfig()'s zero-value defaults first.
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

	return cfg, nil
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
type Problem struct {
	Field   string
	Message string
}

// String renders p as "field: message", the form preflight and the tray's
// startup-error surface both print.
func (p Problem) String() string {
	return fmt.Sprintf("%s: %s", p.Field, p.Message)
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

	// checkPlaceholder's message normally names the matched placeholder
	// (e.g. "${BRANCHDAM_AGENT_API_KEY}") -- genuinely useful for an
	// operator scanning several fields at once, and safe for every field
	// here except one. redact drops that substring instead: server.apiKey
	// is the one field where "the matched text is exactly the substring
	// between literal ${ and }" stops being a safe assumption the moment
	// the value is ever a REAL secret rather than a placeholder (a
	// pathological but not impossible API key containing literal "${...}"
	// characters) -- CodeQL's go/clear-text-logging flagged exactly this
	// path once Validate()'s problems started reaching slog (tray.go), not
	// just fmt.Fprintln (preflight.go, a sink that query doesn't cover).
	checkPlaceholder := func(field, value string, redact bool) {
		m := envVarRe.FindString(value)
		if m == "" {
			return
		}
		msg := fmt.Sprintf("still contains the unexpanded placeholder %s -- the environment variable was not set when this config was loaded", m)
		if redact {
			msg = "still contains an unexpanded placeholder (value withheld) -- the environment variable was not set when this config was loaded"
		}
		problems = append(problems, Problem{Field: field, Message: msg})
	}

	checkPlaceholder("server.baseUrl", c.Server.BaseURL, false)
	checkPlaceholder("server.apiKey", c.Server.APIKey, true)
	checkPlaceholder("agentId", c.AgentID, false)
	checkPlaceholder("ingest.archiveRoot", c.Ingest.ArchiveRoot, false)
	checkPlaceholder("ingest.localEditRoot", c.Ingest.LocalEditRoot, false)
	checkPlaceholder("ingest.pathTemplate", c.Ingest.PathTemplate, false)
	for i, root := range c.Ingest.CardRoots {
		checkPlaceholder(fmt.Sprintf("ingest.cardRoots[%d]", i), root, false)
	}
	checkPlaceholder("offline.queueDbPath", c.Offline.QueueDBPath, false)
	checkPlaceholder("offline.tier0ContainerRoot", c.Offline.Tier0ContainerRoot, false)
	checkPlaceholder("tray.statusAddr", c.Tray.StatusAddr, false)
	checkPlaceholder("selfUpdate.repo", c.SelfUpdate.Repo, false)
	for i, m := range c.PathMappings {
		checkPlaceholder(fmt.Sprintf("pathMappings[%d].workstationPath", i), m.WorkstationPath, false)
		checkPlaceholder(fmt.Sprintf("pathMappings[%d].containerPath", i), m.ContainerPath, false)
	}

	if c.Server.APIKey != "" && !envVarRe.MatchString(c.Server.APIKey) && len(c.Server.APIKey) < 32 {
		problems = append(problems, Problem{
			Field:   "server.apiKey",
			Message: `under 32 characters -- the server rejects this with a 503 ("agent authentication is not configured")`,
		})
	}

	if c.Ingest.PollIntervalSecs < 0 {
		problems = append(problems, Problem{Field: "ingest.pollIntervalSecs", Message: "must not be negative"})
	}
	if c.Prune.MinAgeHours < 0 {
		problems = append(problems, Problem{Field: "prune.minAgeHours", Message: "must not be negative"})
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
