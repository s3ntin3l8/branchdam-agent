package ingest

// TestParityAgentIngestVsServerScan is "the parity test that matters"
// (issue #2's acceptance criteria): it builds a real branchDAM server
// binary from a sibling checkout, builds this repo's own `branchdam-agent`
// binary, runs a normal POST /api/v1/scan over a fixture set in one
// storage location and a headless `ingest --card` run over the identical
// fixture bytes into a second storage location on the SAME server/database,
// and diffs the resulting media_nodes rows for phash, camera_model,
// camera_serial, lens_model, captured_at_unix, filename_stem, fast_hash,
// and full_hash -- exactly the list issue #2 specifies.
//
// This requires a branchDAM (server) checkout on disk, which is NOT part of
// this module and is not checked out by this repo's own CI (there is no
// branchDAM source tree available there) -- so the test is env-gated:
// BRANCHDAM_SRC, or (failing that) a small set of default-probed sibling
// paths for local dev convenience. Skips cleanly, not a failure, when none
// resolve to a real branchDAM checkout. Also requires `exiftool` and
// `go build` on PATH; skips cleanly if either is missing.
//
// See TestExifExtractsPromotedColumnsAndSidecarWins and the djisrt/hashing
// package tests for the narrower, always-run unit coverage of the pieces
// this test wires together end-to-end.
import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parityTestAPIKey must be >= 32 characters (server.apiKey's own
// requirement) -- a fixed, obviously-fake value scoped to this test only.
const parityTestAPIKey = "parity-test-agent-api-key-0123456789" // pragma: allowlist secret

func TestParityAgentIngestVsServerScan(t *testing.T) {
	branchdamSrc := locateBranchDAMSrc(t)
	if branchdamSrc == "" {
		t.Skip("no branchDAM server checkout found (set BRANCHDAM_SRC to a branchDAM checkout to run this test) -- skipping the cross-repo parity test")
	}
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not found on PATH -- skipping parity test")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not found on PATH -- skipping parity test")
	}
	skipIfServerDirty(t, branchdamSrc)

	agentModuleRoot := findModuleRoot(t)

	// --- Build both binaries up front, fail fast if either doesn't build. ---
	serverBin := buildBranchDAMServer(t, branchdamSrc)
	agentBin := buildAgentBinary(t, agentModuleRoot)

	// --- Fixture layout. ---
	dir := t.TempDir()
	cardDir := filepath.Join(dir, "card")
	serverScanDir := filepath.Join(dir, "storage", "serverscan")
	agentArchiveDir := filepath.Join(dir, "storage", "agentarchive")
	for _, d := range []string{cardDir, serverScanDir, agentArchiveDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeParityFixtures(t, cardDir)
	// The server-side baseline scan reads the identical bytes from a
	// SEPARATE directory (not the agent's own destination) so the two code
	// paths never share a single written copy -- copying, not moving,
	// proves each side did its own independent work.
	copyDir(t, cardDir, serverScanDir)

	// --- Start the branchDAM server. ---
	dbPath := filepath.Join(dir, "branchdam.db")
	port := freePort(t)
	cfgPath := filepath.Join(dir, "server-config.yaml")
	writeServerConfig(t, cfgPath, serverConfigVars{
		DBPath:           dbPath,
		Port:             port,
		APIKey:           parityTestAPIKey,
		ThumbsDir:        filepath.Join(dir, "thumbs"),
		ServerScanRoot:   serverScanDir,
		AgentArchiveRoot: agentArchiveDir,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	serverCmd := exec.CommandContext(ctx, serverBin, "-config", cfgPath)
	var serverLog strings.Builder
	serverCmd.Stdout = &serverLog
	serverCmd.Stderr = &serverLog
	if err := serverCmd.Start(); err != nil {
		t.Fatalf("start branchDAM server: %v", err)
	}
	t.Cleanup(func() {
		_ = serverCmd.Process.Kill()
		_ = serverCmd.Wait()
		if t.Failed() {
			t.Logf("branchDAM server log:\n%s", serverLog.String())
		}
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealthz(t, baseURL, 20*time.Second)

	// --- Resolve storage location IDs (seeded from config.yaml at startup). ---
	serverScanLocID := waitForStorageLocationID(t, dbPath, "serverscan", 10*time.Second)
	agentArchiveLocID := waitForStorageLocationID(t, dbPath, "agentarchive", 10*time.Second)

	// --- Run the agent's headless ingest binary against the card fixtures. ---
	agentCfgPath := filepath.Join(dir, "agent-config.yaml")
	writeAgentConfig(t, agentCfgPath, agentConfigVars{
		BaseURL:          baseURL,
		APIKey:           parityTestAPIKey,
		AgentArchiveRoot: agentArchiveDir,
		LocalEditRoot:    filepath.Join(dir, "localedit"),
	})
	runAgentIngest(t, agentBin, agentCfgPath, cardDir)

	// --- Trigger the normal server-side scan over the identical bytes. ---
	triggerServerScan(t, baseURL, serverScanLocID)

	// --- Wait for both sides to finish indexing (async on both: the scan
	// pipeline batches commits, the agent's events drain on a 2s ticker). ---
	wantFiles := []string{"photo.jpg", "photo.arw", "clip.mp4"}
	waitForMediaNodeCount(t, dbPath, serverScanLocID, len(wantFiles), 20*time.Second)
	waitForMediaNodeCount(t, dbPath, agentArchiveLocID, len(wantFiles), 20*time.Second)

	// --- The actual diff. ---
	serverRows := readMediaNodes(t, dbPath, serverScanLocID)
	agentRows := readMediaNodes(t, dbPath, agentArchiveLocID)

	for _, name := range wantFiles {
		sRow, sOK := serverRows[name]
		aRow, aOK := agentRows[name]
		if !sOK {
			t.Errorf("%s: no server-scan media_nodes row", name)
			continue
		}
		if !aOK {
			t.Errorf("%s: no agent-ingest media_nodes row", name)
			continue
		}
		compareParityRow(t, name, sRow, aRow)
	}

	// AC: the DJI .srt fixture produces a GPS-populated video node, with no
	// separate node for the .srt file itself.
	assertNoNodeFor(t, dbPath, agentArchiveLocID, "clip.srt")
	assertGPSMetadata(t, dbPath, agentArchiveLocID, "clip.mp4", 30.335120, -81.655480)
}

// parityRow is the eight-column comparison set issue #2 specifies, plus
// enough identity to look the row up. Pointer fields are nil for SQL NULL
// (sqlite3 -json emits `null`, which json.Unmarshal maps to a nil pointer
// -- no sql.NullXxx wrappers needed since every read here goes through the
// sqlite3 CLI's -json output, not database/sql).
type parityRow struct {
	ID             int64
	PHash          *int64
	CameraModel    *string
	CameraSerial   *string
	LensModel      *string
	CapturedAtUnix *int64
	FilenameStem   *string
	FastHash       *string
	FullHash       *string
}

func (r parityRow) strEq(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func (r parityRow) intEq(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func fmtStrPtr(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

func fmtIntPtr(p *int64) string {
	if p == nil {
		return "<NULL>"
	}
	return fmt.Sprintf("%d", *p)
}

// compareParityRow asserts equality on every promoted column issue #2 lists,
// and -- per the specific failure mode of a vacuously-green test -- first
// asserts that the fields expected to be non-NULL for photo.jpg actually
// are, so a broken fixture (no real EXIF written) would fail loudly instead
// of "passing" on NULL==NULL.
func compareParityRow(t *testing.T, name string, s, a parityRow) {
	t.Helper()

	if name == "photo.jpg" || name == "photo.arw" {
		if s.CameraModel == nil || *s.CameraModel == "" {
			t.Errorf("%s: server-scan CameraModel is NULL/empty -- fixture EXIF was not actually written, this comparison would be vacuous", name)
		}
		if s.CapturedAtUnix == nil {
			t.Errorf("%s: server-scan CapturedAtUnix is NULL -- fixture EXIF was not actually written", name)
		}
		if s.PHash == nil {
			t.Errorf("%s: server-scan PHash is NULL -- fixture image did not decode", name)
		}
	}

	if !s.intEq(s.PHash, a.PHash) {
		t.Errorf("%s: phash mismatch: server=%s agent=%s", name, fmtIntPtr(s.PHash), fmtIntPtr(a.PHash))
	}
	if !s.strEq(s.CameraModel, a.CameraModel) {
		t.Errorf("%s: camera_model mismatch: server=%s agent=%s", name, fmtStrPtr(s.CameraModel), fmtStrPtr(a.CameraModel))
	}
	if !s.strEq(s.CameraSerial, a.CameraSerial) {
		t.Errorf("%s: camera_serial mismatch: server=%s agent=%s", name, fmtStrPtr(s.CameraSerial), fmtStrPtr(a.CameraSerial))
	}
	if !s.strEq(s.LensModel, a.LensModel) {
		t.Errorf("%s: lens_model mismatch: server=%s agent=%s", name, fmtStrPtr(s.LensModel), fmtStrPtr(a.LensModel))
	}
	if !s.intEq(s.CapturedAtUnix, a.CapturedAtUnix) {
		t.Errorf("%s: captured_at_unix mismatch: server=%s agent=%s", name, fmtIntPtr(s.CapturedAtUnix), fmtIntPtr(a.CapturedAtUnix))
	}
	if !s.strEq(s.FilenameStem, a.FilenameStem) {
		t.Errorf("%s: filename_stem mismatch: server=%s agent=%s", name, fmtStrPtr(s.FilenameStem), fmtStrPtr(a.FilenameStem))
	}
	if !s.strEq(s.FastHash, a.FastHash) {
		t.Errorf("%s: fast_hash mismatch: server=%s agent=%s", name, fmtStrPtr(s.FastHash), fmtStrPtr(a.FastHash))
	}
	if !s.strEq(s.FullHash, a.FullHash) {
		t.Errorf("%s: full_hash mismatch: server=%s agent=%s", name, fmtStrPtr(s.FullHash), fmtStrPtr(a.FullHash))
	}
}

// --- fixture construction ---

func writeParityFixtures(t *testing.T, cardDir string) {
	t.Helper()
	exiftoolPath := requireExiftool(t)

	tags := map[string]string{
		"EXIF:Model":              "ILCE-7RM4",
		"EXIF:SerialNumber":       "PARITY-SERIAL-1",
		"EXIF:LensModel":          "FE 24-70mm F2.8 GM",
		"EXIF:DateTimeOriginal":   "2024:03:20 12:59:17",
		"EXIF:OffsetTimeOriginal": "-04:00",
	}

	jpegPath := filepath.Join(cardDir, "photo.jpg")
	if err := makeMinimalJPEG(jpegPath); err != nil {
		t.Fatalf("create photo.jpg fixture: %v", err)
	}
	writeTags(t, exiftoolPath, jpegPath, tags)

	// "RAW" stand-in: the SAME already-tagged JPEG bytes, copied under a
	// .arw extension. This is a deliberate, documented substitute for a
	// real camera RAW file (none was available -- see issue #2's report on
	// this). Tags are written to the .jpg first and the tagged bytes copied
	// over, rather than writing tags directly into the .arw-named copy:
	// exiftool's writer validates file structure against the extension
	// ("Not a valid ARW (looks more like a JPEG)") and refuses to write --
	// but that check is write-path only, so reading it back (this test's
	// Exif() calls, and the server's own probe.Exif) still content-sniffs
	// correctly regardless of the misleading extension. This exercises
	// isImageExt("arw")==true on both sides and Go's image.Decode content
	// sniffing (which ignores the extension, decoding the real JPEG bytes
	// underneath), so phash/camera-field parity for a RAW-classified
	// extension IS covered; what is NOT covered here is the
	// direct-decode-fails-fall-back-to-exiftool-preview-extraction branch a
	// real, undecodable RAW would exercise -- that branch has its own
	// dedicated unit coverage in internal/phash/phash_test.go (a fake
	// exiftool script asserting the fallback order), ported from M0.
	arwPath := filepath.Join(cardDir, "photo.arw")
	jpegBytes, err := os.ReadFile(jpegPath)
	if err != nil {
		t.Fatalf("read tagged photo.jpg: %v", err)
	}
	if err := os.WriteFile(arwPath, jpegBytes, 0o644); err != nil {
		t.Fatalf("create photo.arw fixture: %v", err)
	}

	// Video + DJI telemetry pair. The MP4 content itself doesn't need to be
	// a real, playable video for this test: ffprobe/exiftool both fail
	// gracefully (logged, non-fatal) against non-video bytes on the server
	// side, and none of the eight compared columns depend on stream
	// metadata (duration/codec/etc. aren't in the promoted-column set).
	if err := os.WriteFile(filepath.Join(cardDir, "clip.mp4"), []byte("not-a-real-mp4-but-thats-fine-for-parity-purposes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Real DJI bracket-format telemetry, first fix (30.335120, -81.655480)
	// -- same fixture content branchDAM's own internal/djisrt/testdata/sample.srt
	// uses, so the parsed value is a known quantity.
	srtContent := "1\n00:00:00,000 --> 00:00:00,033\n" +
		"<font size=\"28\">FrameCnt: 1, DiffTime: 33ms\n" +
		"2024-03-20 12:59:17,819\n" +
		"[iso: 400] [shutter: 1/320.0] [fnum: 1.7] [latitude: 30.335120] [longitude: -81.655480] [rel_alt: 6.500 abs_alt: -32.309]</font>\n"
	if err := os.WriteFile(filepath.Join(cardDir, "clip.srt"), []byte(srtContent), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// --- branchDAM discovery/build ---

func locateBranchDAMSrc(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("BRANCHDAM_SRC"); v != "" {
		if isBranchDAMServerModule(v) {
			return v
		}
		t.Fatalf("BRANCHDAM_SRC=%s does not look like a branchDAM server checkout (expected go.mod with module github.com/s3ntin3l8/branchdam)", v)
	}

	root := findModuleRoot(t)
	candidates := []string{
		filepath.Join(root, "..", "branchDAM"),
		filepath.Join(root, "..", "..", "branchDAM"), // worktree layout: <parent>/branchdam-agent-wt/<n>
	}
	for _, c := range candidates {
		if isBranchDAMServerModule(c) {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}

func isBranchDAMServerModule(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	first := strings.SplitN(string(data), "\n", 2)[0]
	return strings.TrimSpace(first) == "module github.com/s3ntin3l8/branchdam"
}

// skipIfServerDirty skips the parity test if the server checkout has
// uncommitted changes in paths that feed `go build` (sqlc-generated
// code, sqlc sources, hashing). Unrelated WIP (e.g. a stray edit in
// docs/) does not mask parity coverage. Returns false for non-git
// directories or when git isn't available.
func skipIfServerDirty(t *testing.T, dir string) {
	t.Helper()
	if dirty, path := hasDirtyBuildPaths(dir); dirty {
		t.Skipf("skipping parity test: server checkout at %s has uncommitted changes in %s; commit or stash first", dir, path)
	}
}

// buildPathsThatAffectCompilation are the server-side paths whose
// uncommitted state can cause `go build ./...` to fail. Keeping this
// list narrow means unrelated WIP in the sibling checkout doesn't
// silently skip the parity test.
var buildPathsThatAffectCompilation = []string{
	"internal/db/sqlcgen/",
	"internal/db/queries/",
	"internal/hashing/",
	"internal/httpapi/",
}

// hasDirtyBuildPaths returns (true, firstDirtyPath) if any of the
// paths that feed `go build` have uncommitted changes (staged,
// unstaged, or untracked). Returns ("", "") for clean or non-git.
func hasDirtyBuildPaths(dir string) (bool, string) {
	args := append([]string{"-C", dir, "status", "--porcelain", "--"}, buildPathsThatAffectCompilation...)
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return false, "" // not a git repo or git not available
	}
	raw := string(out)
	// Trim trailing whitespace/newlines but preserve the leading status
	// characters (XY ) so the path offset is predictable.
	raw = strings.TrimRight(raw, " \t\n\r")
	if len(raw) == 0 {
		return false, ""
	}
	// Return the first dirty path for the skip message.
	firstLine := strings.SplitN(raw, "\n", 2)[0]
	// git status --porcelain format: XY <path> (2 status chars + space + path)
	path := strings.TrimSpace(firstLine[3:])
	return true, path
}

// TestHasDirtyBuildPaths verifies the narrowed dirty-check helper using
// a real git repository in a temp directory. The check only flags paths
// that feed `go build` (sqlcgen, queries, hashing, httpapi), not
// unrelated WIP.
func TestHasDirtyBuildPaths(t *testing.T) {
	// Dirty in a build path → true.
	dirty := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = dirty
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dirty, "internal", "db", "sqlcgen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirty, "internal", "db", "sqlcgen", "querier.go"), []byte("package sqlcgen"), 0o644); err != nil {
		t.Fatal(err)
	}
	isDirty, path := hasDirtyBuildPaths(dirty)
	if !isDirty {
		t.Error("hasDirtyBuildPaths returned false for dirty build path")
	}
	if path == "" {
		t.Error("hasDirtyBuildPaths returned empty path for dirty build path")
	}

	// Dirty outside build paths → false.
	unrelated := t.TempDir()
	cmd = exec.Command("git", "init")
	cmd.Dir = unrelated
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "README.md"), []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, _ := hasDirtyBuildPaths(unrelated); d {
		t.Error("hasDirtyBuildPaths returned true for unrelated WIP")
	}

	// Clean repo → false.
	clean := t.TempDir()
	cmd = exec.Command("git", "init")
	cmd.Dir = clean
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if d, _ := hasDirtyBuildPaths(clean); d {
		t.Error("hasDirtyBuildPaths returned true for clean repo")
	}

	// Non-git directory → false.
	nogit := t.TempDir()
	if d, _ := hasDirtyBuildPaths(nogit); d {
		t.Error("hasDirtyBuildPaths returned true for non-git directory")
	}
}

// findModuleRoot walks up from the test's working directory until it finds
// this module's own go.mod (module github.com/s3ntin3l8/branchdam-agent).
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module github.com/s3ntin3l8/branchdam-agent") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find branchdam-agent module root by walking up from cwd")
		}
		dir = parent
	}
}

func buildBranchDAMServer(t *testing.T, src string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(src, "web", "dist", "index.html")); err != nil {
		cmd := exec.Command("bash", ".github/ci-prebuild.sh")
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("branchDAM ci-prebuild.sh: %v\n%s", err, out)
		}
	}
	bin := filepath.Join(t.TempDir(), "branchdam-server")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/branchdam")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build branchDAM server: %v\n%s", err, out)
	}
	return bin
}

func buildAgentBinary(t *testing.T, moduleRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "branchdam-agent")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/branchdam-agent")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build branchdam-agent: %v\n%s", err, out)
	}
	return bin
}

// --- config rendering ---

type serverConfigVars struct {
	DBPath           string
	Port             int
	APIKey           string
	ThumbsDir        string
	ServerScanRoot   string
	AgentArchiveRoot string
}

func writeServerConfig(t *testing.T, path string, v serverConfigVars) {
	t.Helper()
	content := fmt.Sprintf(`
listenAddr: "127.0.0.1:%d"
logLevel: error
database:
  path: %s
http:
  readTimeoutSecs: 15
  writeTimeoutSecs: 15
  exposeOpenAPI: false
workers:
  hashWorkers: 0
  fullHashPolicy: tier3_and_collision
thumbnails:
  cacheDir: %s
agent:
  apiKey: %q
authz:
  groups:
    - parity-test-admins
immich:
  apiUrl: ""
storageLocations:
  - name: serverscan
    rootPath: %s
    tier: TIER3_MASTER_ARCHIVE
    readOnly: true
  - name: agentarchive
    rootPath: %s
    tier: TIER3_MASTER_ARCHIVE
    readOnly: true
`, v.Port, v.DBPath, v.ThumbsDir, v.APIKey, v.ServerScanRoot, v.AgentArchiveRoot)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type agentConfigVars struct {
	BaseURL          string
	APIKey           string
	AgentArchiveRoot string
	LocalEditRoot    string
}

func writeAgentConfig(t *testing.T, path string, v agentConfigVars) {
	t.Helper()
	content := fmt.Sprintf(`
server:
  baseUrl: %q
  apiKey: %q
agentId: "parity-test-agent"
pathMappings:
  - workstationPath: %q
    containerPath: %q
ingest:
  archiveRoot: %q
  localEditRoot: %q
  pathTemplate: "{original_name}"
`, v.BaseURL, v.APIKey, v.AgentArchiveRoot, v.AgentArchiveRoot, v.AgentArchiveRoot, v.LocalEditRoot)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- process/network helpers ---

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForHealthz(t *testing.T, baseURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("branchDAM server at %s never became healthy within %s", baseURL, timeout)
}

func runAgentIngest(t *testing.T, agentBin, cfgPath, cardDir string) {
	t.Helper()
	cmd := exec.Command(agentBin, "ingest", "-config", cfgPath, "-card", cardDir, "-timeout", "60s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("branchdam-agent ingest failed: %v\n%s", err, out)
	}
	t.Logf("branchdam-agent ingest output:\n%s", out)
}

func triggerServerScan(t *testing.T, baseURL string, locationID int64) {
	t.Helper()
	body := fmt.Sprintf(`{"storageLocationId":%d}`, locationID)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/scan", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// No Traefik/Authentik in front of this bare test server -- setting
	// these headers directly is the same pattern branchDAM's own
	// routes_test.go uses to exercise BrowserChain-protected routes without
	// a real Authentik instance.
	req.Header.Set("X-Authentik-Username", "parity-test-user")
	req.Header.Set("X-Authentik-Groups", "parity-test-admins")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/scan: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("POST /api/v1/scan: status %d", resp.StatusCode)
	}
}

// --- DB helpers (shell out to the sqlite3 CLI, read-only) ---
//
// Reading branchDAM's SQLite file for assertions here shells out to the
// `sqlite3` CLI (`-json` output mode) rather than adding a Go SQLite driver
// dependency to this module -- this repo has no SQLite dependency yet (M2's
// offline queue, `modernc.org/sqlite`, is a separate, later, *production*
// addition), and a test-only reader doesn't justify pulling one in early
// just to avoid a subprocess call. `sqlite3` is a common dev-machine tool;
// this test already requires `exiftool` and a `go` toolchain on PATH and
// skips cleanly when either is absent, so gating on `sqlite3` too (checked
// implicitly by the first query failing to find the binary) fits the same
// pattern.

// sqlJSON runs a single read-only query against dbPath via the sqlite3 CLI
// and unmarshals its -json output into dest (a pointer to a slice of
// structs/maps, matching encoding/json's own Unmarshal contract).
func sqlJSON(t *testing.T, dbPath, query string, dest any) error {
	t.Helper()
	cmd := exec.Command("sqlite3", "-json", "file:"+dbPath+"?mode=ro", query)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("sqlite3 query %q: %v (stderr: %s)", query, err, ee.Stderr)
		}
		return fmt.Errorf("sqlite3 query %q: %w (is the sqlite3 CLI installed?)", query, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		trimmed = "[]"
	}
	return json.Unmarshal([]byte(trimmed), dest)
}

func waitForStorageLocationID(t *testing.T, dbPath, name string, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		var rows []struct {
			ID int64 `json:"id"`
		}
		err := sqlJSON(t, dbPath, fmt.Sprintf(`SELECT id FROM storage_locations WHERE name = '%s'`, sqlEscape(name)), &rows)
		if err == nil && len(rows) == 1 {
			return rows[0].ID
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("storage location %q never appeared within %s: %v", name, timeout, lastErr)
	return 0
}

func waitForMediaNodeCount(t *testing.T, dbPath string, locationID int64, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got int
	for time.Now().Before(deadline) {
		var rows []struct {
			N int `json:"n"`
		}
		q := fmt.Sprintf(`SELECT COUNT(*) AS n FROM media_nodes WHERE storage_location_id = %d AND lifecycle_state = 'ACTIVE'`, locationID)
		if err := sqlJSON(t, dbPath, q, &rows); err == nil && len(rows) == 1 {
			got = rows[0].N
			if got >= want {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("storage_location_id=%d: only %d/%d media_nodes rows appeared within %s", locationID, got, want, timeout)
}

func readMediaNodes(t *testing.T, dbPath string, locationID int64) map[string]parityRow {
	t.Helper()
	var rows []struct {
		ID             int64   `json:"id"`
		FileName       string  `json:"file_name"`
		PHash          *int64  `json:"phash"`
		CameraModel    *string `json:"camera_model"`
		CameraSerial   *string `json:"camera_serial"`
		LensModel      *string `json:"lens_model"`
		CapturedAtUnix *int64  `json:"captured_at_unix"`
		FilenameStem   *string `json:"filename_stem"`
		FastHash       *string `json:"fast_hash"`
		FullHash       *string `json:"full_hash"`
	}
	q := fmt.Sprintf(`
		SELECT id, file_name, phash, camera_model, camera_serial, lens_model,
		       captured_at_unix, filename_stem, fast_hash, full_hash
		FROM media_nodes
		WHERE storage_location_id = %d AND lifecycle_state = 'ACTIVE'`, locationID)
	if err := sqlJSON(t, dbPath, q, &rows); err != nil {
		t.Fatalf("query media_nodes: %v", err)
	}

	out := map[string]parityRow{}
	for _, r := range rows {
		out[r.FileName] = parityRow{
			ID: r.ID, PHash: r.PHash, CameraModel: r.CameraModel, CameraSerial: r.CameraSerial,
			LensModel: r.LensModel, CapturedAtUnix: r.CapturedAtUnix, FilenameStem: r.FilenameStem,
			FastHash: r.FastHash, FullHash: r.FullHash,
		}
	}
	return out
}

func assertNoNodeFor(t *testing.T, dbPath string, locationID int64, fileName string) {
	t.Helper()
	var rows []struct {
		N int `json:"n"`
	}
	q := fmt.Sprintf(`SELECT COUNT(*) AS n FROM media_nodes WHERE storage_location_id = %d AND file_name = '%s'`, locationID, sqlEscape(fileName))
	if err := sqlJSON(t, dbPath, q, &rows); err != nil {
		t.Fatalf("query media_nodes for %s: %v", fileName, err)
	}
	if len(rows) != 1 || rows[0].N != 0 {
		t.Errorf("expected no media_nodes row for %s (agent must not submit an event for a .srt sidecar), found %+v", fileName, rows)
	}
}

func assertGPSMetadata(t *testing.T, dbPath string, locationID int64, fileName string, wantLat, wantLon float64) {
	t.Helper()
	var idRows []struct {
		ID int64 `json:"id"`
	}
	q := fmt.Sprintf(`SELECT id FROM media_nodes WHERE storage_location_id = %d AND file_name = '%s'`, locationID, sqlEscape(fileName))
	if err := sqlJSON(t, dbPath, q, &idRows); err != nil || len(idRows) != 1 {
		t.Fatalf("find node id for %s: err=%v rows=%v", fileName, err, idRows)
	}
	nodeID := idRows[0].ID

	var metaRows []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	q2 := fmt.Sprintf(`SELECT key, value FROM node_metadata WHERE node_id = %d AND key IN ('Composite:GPSLatitude','Composite:GPSLongitude')`, nodeID)
	if err := sqlJSON(t, dbPath, q2, &metaRows); err != nil {
		t.Fatalf("query node_metadata: %v", err)
	}

	got := map[string]string{}
	for _, r := range metaRows {
		got[r.Key] = r.Value
	}
	if got["Composite:GPSLatitude"] == "" || got["Composite:GPSLongitude"] == "" {
		t.Fatalf("%s: no GPS node_metadata found (want lat/lon from the DJI .srt fixture), got %v", fileName, got)
	}
	var gotLat, gotLon float64
	if _, err := fmt.Sscanf(got["Composite:GPSLatitude"], "%g", &gotLat); err != nil {
		t.Fatalf("parse stored GPSLatitude %q: %v", got["Composite:GPSLatitude"], err)
	}
	if _, err := fmt.Sscanf(got["Composite:GPSLongitude"], "%g", &gotLon); err != nil {
		t.Fatalf("parse stored GPSLongitude %q: %v", got["Composite:GPSLongitude"], err)
	}
	const eps = 1e-6
	if abs(gotLat-wantLat) > eps || abs(gotLon-wantLon) > eps {
		t.Errorf("%s: GPS = (%v, %v), want (%v, %v)", fileName, gotLat, gotLon, wantLat, wantLon)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// sqlEscape escapes a single-quoted SQL string literal for the fixed,
// test-internal set of file names this test ever interpolates (never
// user/attacker input) -- doubling embedded single quotes is sufficient for
// that closed set.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
