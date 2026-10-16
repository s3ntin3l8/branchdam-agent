package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// forceBufferedVerify makes every Verify in this test report the buffered
// floor, whatever the temp dir's filesystem supports.
func forceBufferedVerify(t *testing.T) {
	t.Helper()
	orig := openForVerifyFn
	openForVerifyFn = func(path string) (io.ReadCloser, VerifyMethod, error) {
		f, err := os.Open(path) //nolint:gosec // test temp file
		if err != nil {
			return nil, "", err
		}
		return f, VerifyMethodBufferedFloor, nil
	}
	t.Cleanup(func() { openForVerifyFn = orig })
}

// scriptedClient is a nodeCreator (and optionally a contentChecker via
// check) whose POST outcome is decided per call by post.
type scriptedClient struct {
	post  func(ctx context.Context, payload branchdam.NodeCreatedPayload, eventUUID string) (*branchdam.EventResponse, error)
	check func(q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error)

	posts      []branchdam.NodeCreatedPayload
	checkCalls []branchdam.ContentCheckQuery // every CheckContent query, in order
}

func (c *scriptedClient) PostNodeCreatedWithUUID(ctx context.Context, _ string, payload branchdam.NodeCreatedPayload, eventUUID string) (*branchdam.EventResponse, error) {
	c.posts = append(c.posts, payload)
	if c.post != nil {
		return c.post(ctx, payload, eventUUID)
	}
	return &branchdam.EventResponse{EventID: "evt-" + payload.NodeUUID}, nil
}

type scriptedCheckClient struct{ *scriptedClient }

// The fake must stay a contentChecker: a signature drift would silently
// turn every content check in these tests into "no checker".
var _ contentChecker = scriptedCheckClient{}

func (c scriptedCheckClient) CheckContent(_ context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
	c.checkCalls = append(c.checkCalls, q)
	if c.check != nil {
		return c.check(q)
	}
	return branchdam.ContentCheckResult{}, nil
}

func writeCardFile(t *testing.T, cardRoot, name string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cardRoot, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func onlyRow(t *testing.T, store *queue.Store) queue.Record {
	t.Helper()
	rows, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("queue.db has %d rows, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// A POST that fails after both copies landed must not lose the file: the
// intent is in queue.db (written before the POST), the file is reported
// Queued rather than failed, and the next Drain pass registers it.
func TestIngestCardQueuesRegistrationWhenPostFails(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeCardFile(t, cardRoot, "IMG_0001.jpg", []byte("photo-bytes"))

	store := openStoreT(t)
	client := &scriptedClient{}
	var inlineEventUUID string
	client.post = func(_ context.Context, _ branchdam.NodeCreatedPayload, eventUUID string) (*branchdam.EventResponse, error) {
		inlineEventUUID = eventUUID
		// Invariant 3: the row is already durable when the POST goes out.
		if rows, err := store.All(context.Background()); err != nil || len(rows) != 1 {
			t.Errorf("at POST time queue.db has %d rows (err=%v), want the file's row already inserted", len(rows), err)
		}
		return nil, errors.New("server unreachable")
	}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	e.Queue = store

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	fr := res.Files[0]
	if fr.Err != nil || fr.Skipped || !fr.Queued || fr.EventID != "" {
		t.Fatalf("result = {Err:%v Skipped:%v Queued:%v EventID:%q}, want queued for drain, not failed", fr.Err, fr.Skipped, fr.Queued, fr.EventID)
	}

	row := onlyRow(t, store)
	if row.Origin != queue.OriginOnline || row.NodeCreatedStatus != queue.StatusPending ||
		row.ArchiveCopyStatus != queue.StatusDone || row.RebaseStatus != queue.StatusSkipped {
		t.Fatalf("row = %+v, want ONLINE with node_created PENDING, archive_copy DONE, rebase SKIPPED", row)
	}
	if row.NodeCreatedAttempts != 1 || !strings.Contains(row.NodeCreatedLastError, "server unreachable") {
		t.Errorf("failed attempt not recorded: attempts=%d lastError=%q", row.NodeCreatedAttempts, row.NodeCreatedLastError)
	}
	if row.ArchiveContainerPath != "/storage/archive/IMG_0001.jpg" || row.LocalPath != fr.LocalPath {
		t.Errorf("row paths = %q / %q", row.ArchiveContainerPath, row.LocalPath)
	}
	if counts, err := store.Counts(context.Background()); err != nil || counts.Pending() != 1 {
		t.Errorf("Counts() = %+v (err=%v), want the unregistered file counted as pending", counts, err)
	}

	// Drain (after the backoff) registers it with the same identity.
	dc := &fakeDrainClient{}
	stats, err := Drain(context.Background(), dc, store, "test-agent", fixedNow(time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if stats.NodeCreatedSent != 1 || len(dc.nodeCreatedCalls) != 1 {
		t.Fatalf("Drain sent %d EVENT_NODE_CREATED, want 1", len(dc.nodeCreatedCalls))
	}
	if got := dc.nodeCreatedCalls[0]; got.NodeUUID != row.NodeUUID || got.FilePath != "/storage/archive/IMG_0001.jpg" {
		t.Errorf("drained payload = %+v, want node %s at the archive path", got, row.NodeUUID)
	}
	if dc.nodeCreatedUUIDCalls[0] != inlineEventUUID {
		t.Errorf("Drain event UUID %q, want the inline attempt's %q (server-side idempotency)", dc.nodeCreatedUUIDCalls[0], inlineEventUUID)
	}
	if len(dc.rebaseCalls) != 0 {
		t.Errorf("Drain rebased an online row %d times; its payload already targets Tier 3", len(dc.rebaseCalls))
	}
	if stats.Remaining != 0 {
		t.Errorf("Remaining = %d after the registration was accepted, want 0", stats.Remaining)
	}
}

// Without a queue a failed POST is still an error, and the copies it left
// are registered by the next run instead of being skipped forever.
func TestIngestCardReingestRegistersCopiesAFailedRunLeft(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeCardFile(t, cardRoot, "IMG_0002.jpg", []byte("photo-bytes-2"))

	client := &scriptedClient{}
	client.post = func(context.Context, branchdam.NodeCreatedPayload, string) (*branchdam.EventResponse, error) {
		return nil, errors.New("server unreachable")
	}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

	res1, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Files[0].Err == nil {
		t.Fatal("first run: expected the failed POST to fail the file when there is no queue")
	}

	client.post = nil // server back
	res2, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res2.Files[0]
	if fr.Err != nil || fr.Skipped {
		t.Fatalf("second run: Err=%v Skipped=%v (%q), want the copies registered", fr.Err, fr.Skipped, fr.SkipReason)
	}
	if fr.EventID == "" || len(client.posts) != 2 {
		t.Fatalf("second run: EventID=%q posts=%d, want a registration", fr.EventID, len(client.posts))
	}
	if got := client.posts[1]; got.FullHash == nil || *got.FullHash != blake3Hex(t, filepath.Join(cardRoot, "IMG_0002.jpg")) {
		t.Errorf("registered full hash = %v, want the card file's", got.FullHash)
	}
}

// A queue row for the destination is enough to skip; the server is not
// asked and nothing is re-posted.
func TestIngestCardReingestSkipsWhenQueueRowClaimsCopies(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeCardFile(t, cardRoot, "IMG_0003.jpg", []byte("photo-bytes-3"))

	base := &scriptedClient{}
	client := scriptedCheckClient{base}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	e.Queue = openStoreT(t)
	// Queued but not yet accepted: still a claim -- Drain owns it.
	base.post = func(context.Context, branchdam.NodeCreatedPayload, string) (*branchdam.EventResponse, error) {
		return nil, errors.New("503")
	}

	if _, err := e.IngestCard(context.Background(), cardRoot); err != nil {
		t.Fatal(err)
	}
	base.post = nil
	base.checkCalls = nil
	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if !fr.Skipped || !strings.Contains(fr.SkipReason, "queued") || fr.NodeUUID != "0000000-fake-uuid" {
		t.Errorf("result = {Skipped:%v SkipReason:%q NodeUUID:%q}, want a skip naming the queued registration", fr.Skipped, fr.SkipReason, fr.NodeUUID)
	}
	if len(base.posts) != 1 {
		t.Errorf("posts = %d, want only the first run's", len(base.posts))
	}
	if len(base.checkCalls) != 0 {
		t.Errorf("CheckContent called %d times, want 0 when a queue row claims the copies", len(base.checkCalls))
	}
}

// Copies with no queue row are skipped when the server has a live node
// with their content -- and only then.
func TestIngestCardReingestSkipsWhenServerHasContent(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	content := []byte("photo-bytes-4")
	writeCardFile(t, cardRoot, "IMG_0004.jpg", content)
	archiveRoot, localRoot := filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	for _, root := range []string{archiveRoot, localRoot} {
		writeCardFile(t, root, "IMG_0004.jpg", content)
	}
	want := blake3Hex(t, filepath.Join(localRoot, "IMG_0004.jpg"))

	base := &scriptedClient{}
	base.check = func(q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		if q.FullHash == want {
			return branchdam.ContentCheckResult{Found: true, NodeUUID: "node-on-server", LifecycleState: "ACTIVE"}, nil
		}
		return branchdam.ContentCheckResult{}, nil
	}
	e := newTestEngine(t, scriptedCheckClient{base}, archiveRoot, localRoot)
	e.Queue = openStoreT(t)

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if fr.Err != nil || !fr.Skipped || fr.ExistingNodeUUID != "node-on-server" {
		t.Fatalf("result = {Err:%v Skipped:%v ExistingNodeUUID:%q}, want a skip on the server's node", fr.Err, fr.Skipped, fr.ExistingNodeUUID)
	}
	if len(base.posts) != 0 {
		t.Errorf("posts = %d, want 0", len(base.posts))
	}
}

// seedPresentCopies writes content under name to the card and to both
// destinations, so the next IngestCard finds the copies already in place
// (ResolveDestination's AlreadyIngested) and goes through confirmRegistered.
func seedPresentCopies(t *testing.T, cardRoot, archiveRoot, localRoot, name string, content []byte) {
	t.Helper()
	for _, root := range []string{cardRoot, archiveRoot, localRoot} {
		writeCardFile(t, root, name, content)
	}
}

// confirmRegistered already has the local copy's full hash, so it asks the
// definitive full-hash lookup straight away (with fastHash and sizeBytes),
// never the fast pre-screen.
func TestIngestCardReingestQueriesFullHashWithFastHashAndSize(t *testing.T) {
	dir := t.TempDir()
	cardRoot, archiveRoot, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	content := []byte("photo-bytes-7")
	seedPresentCopies(t, cardRoot, archiveRoot, localRoot, "IMG_0007.jpg", content)
	localCopy := filepath.Join(localRoot, "IMG_0007.jpg")
	wantFull := blake3Hex(t, localCopy)
	wantFast, err := fastHashFile(localCopy, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}

	base := &scriptedClient{}
	base.check = func(branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		return branchdam.ContentCheckResult{Found: false}, nil
	}
	e := newTestEngine(t, scriptedCheckClient{base}, archiveRoot, localRoot)

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if fr.Err != nil || fr.Skipped || fr.EventID == "" {
		t.Fatalf("result = {Err:%v Skipped:%v EventID:%q}, want the unregistered copies registered", fr.Err, fr.Skipped, fr.EventID)
	}
	want := branchdam.ContentCheckQuery{FastHash: wantFast, SizeBytes: int64(len(content)), FullHash: wantFull}
	if len(base.checkCalls) != 1 || base.checkCalls[0] != want {
		t.Errorf("CheckContent queries = %+v, want exactly [%+v]", base.checkCalls, want)
	}
}

// A 400 on confirmRegistered's lookup means a server predating the
// pre-screen: the lookup is retried once as fastHash+fullHash (the form such
// servers accept), the answer is honoured, and the rest of the pass uses
// that form straight away -- a 400 never makes dedup "unavailable".
func TestIngestCardReingestFallsBackToFullHashOnlyOn400(t *testing.T) {
	dir := t.TempDir()
	cardRoot, archiveRoot, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	seedPresentCopies(t, cardRoot, archiveRoot, localRoot, "IMG_0008.jpg", []byte("photo-bytes-8"))
	seedPresentCopies(t, cardRoot, archiveRoot, localRoot, "IMG_0009.jpg", []byte("photo-bytes-9"))
	hash8 := blake3Hex(t, filepath.Join(localRoot, "IMG_0008.jpg"))
	hash9 := blake3Hex(t, filepath.Join(localRoot, "IMG_0009.jpg"))

	base := &scriptedClient{}
	base.check = func(q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		if q.SizeBytes != 0 {
			return branchdam.ContentCheckResult{}, &branchdam.HTTPError{StatusCode: 400, Body: "unknown parameter sizeBytes"}
		}
		switch q.FullHash {
		case hash8:
			return branchdam.ContentCheckResult{Found: true, NodeUUID: "node-8", FilePath: "/elsewhere/IMG_0008.jpg", LifecycleState: "ACTIVE"}, nil
		case hash9:
			return branchdam.ContentCheckResult{Found: true, NodeUUID: "node-9", FilePath: "/elsewhere/IMG_0009.jpg", LifecycleState: "HIDDEN"}, nil
		}
		return branchdam.ContentCheckResult{}, nil
	}
	e := newTestEngine(t, scriptedCheckClient{base}, archiveRoot, localRoot)

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(res.Files))
	}
	for i, wantNode := range []string{"node-8", "node-9"} {
		fr := res.Files[i]
		if fr.Err != nil || !fr.Skipped || fr.ExistingNodeUUID != wantNode {
			t.Errorf("file %d = {Err:%v Skipped:%v ExistingNodeUUID:%q}, want a skip on %s (HIDDEN counts as live)", i, fr.Err, fr.Skipped, fr.ExistingNodeUUID, wantNode)
		}
	}
	if len(base.posts) != 0 {
		t.Errorf("posts = %d, want 0", len(base.posts))
	}
	// File 1: the full query (400), then the retry without sizeBytes.
	// File 2: straight to the retry form -- fullOnly latched for the pass.
	if len(base.checkCalls) != 3 {
		t.Fatalf("CheckContent queries = %+v, want 3 (400, retry, retry form)", base.checkCalls)
	}
	if base.checkCalls[0].SizeBytes == 0 || base.checkCalls[1].SizeBytes != 0 || base.checkCalls[2].SizeBytes != 0 {
		t.Errorf("CheckContent queries = %+v, want sizeBytes only on the first", base.checkCalls)
	}
	for i, q := range base.checkCalls {
		if q.FullHash == "" || q.FastHash == "" {
			t.Errorf("query %d = %+v, want fastHash and fullHash on every lookup", i, q)
		}
	}
	for _, name := range []string{"IMG_0008.jpg", "IMG_0009.jpg"} {
		for _, root := range []string{archiveRoot, localRoot} {
			if _, err := os.Stat(filepath.Join(root, name)); err != nil {
				t.Errorf("pre-existing copy %s removed: %v", filepath.Join(root, name), err)
			}
		}
	}
}

// When check-content is missing (404), confirmRegistered fails open and
// registers; the rest of the pass does not ask again.
func TestIngestCardReingestRegistersWhenContentCheckUnavailable(t *testing.T) {
	dir := t.TempDir()
	cardRoot, archiveRoot, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	seedPresentCopies(t, cardRoot, archiveRoot, localRoot, "IMG_0010.jpg", []byte("photo-bytes-10"))
	seedPresentCopies(t, cardRoot, archiveRoot, localRoot, "IMG_0011.jpg", []byte("photo-bytes-11"))

	base := &scriptedClient{}
	base.check = func(branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		return branchdam.ContentCheckResult{}, &branchdam.HTTPError{StatusCode: 404, Body: "not found"}
	}
	e := newTestEngine(t, scriptedCheckClient{base}, archiveRoot, localRoot)

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	for i, fr := range res.Files {
		if fr.Err != nil || fr.Skipped || fr.EventID == "" {
			t.Errorf("file %d = {Err:%v Skipped:%v EventID:%q}, want registered (fail-open)", i, fr.Err, fr.Skipped, fr.EventID)
		}
	}
	if len(base.posts) != 2 {
		t.Errorf("posts = %d, want 2", len(base.posts))
	}
	if len(base.checkCalls) != 1 {
		t.Errorf("CheckContent called %d times, want 1: a 404 latches unavailable for the pass", len(base.checkCalls))
	}
}

// A live node at this file's own archive container path is this very copy,
// already registered (e.g. the server's own scan of the archive): it is
// evidence of registration -- skipped as this file's node, never reported
// as a duplicate of something else, and nothing is removed or re-posted.
func TestIngestCardReingestOwnPathMatchIsRegistrationNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	cardRoot, archiveRoot, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	content := []byte("photo-bytes-12")
	seedPresentCopies(t, cardRoot, archiveRoot, localRoot, "IMG_0012.jpg", content)

	base := &scriptedClient{}
	base.check = func(branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		return branchdam.ContentCheckResult{Found: true, NodeUUID: "node-own", FilePath: "/storage/archive/IMG_0012.jpg", LifecycleState: "ACTIVE"}, nil
	}
	e := newTestEngine(t, scriptedCheckClient{base}, archiveRoot, localRoot)

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if fr.Err != nil || !fr.Skipped || fr.NodeUUID != "node-own" || fr.ExistingNodeUUID != "" {
		t.Fatalf("result = {Err:%v Skipped:%v NodeUUID:%q ExistingNodeUUID:%q}, want a skip as this file's own registration", fr.Err, fr.Skipped, fr.NodeUUID, fr.ExistingNodeUUID)
	}
	if !strings.Contains(fr.SkipReason, "registered as node node-own") || strings.Contains(fr.SkipReason, "duplicate") {
		t.Errorf("SkipReason = %q, want it to name the registration, not a duplicate", fr.SkipReason)
	}
	if len(base.posts) != 0 {
		t.Errorf("posts = %d, want 0", len(base.posts))
	}
	for _, root := range []string{archiveRoot, localRoot} {
		if got, err := os.ReadFile(filepath.Join(root, "IMG_0012.jpg")); err != nil || !bytes.Equal(got, content) {
			t.Errorf("%s: pre-existing copy changed or removed (err=%v)", root, err)
		}
	}
}

// Copies that are present but not known to be registered are only
// registered after an unbuffered verify when the config requires one.
func TestIngestCardReingestHonoursRequireUnbuffered(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	content := []byte("photo-bytes-5")
	writeCardFile(t, cardRoot, "IMG_0005.jpg", content)
	archiveRoot, localRoot := filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	for _, root := range []string{archiveRoot, localRoot} {
		writeCardFile(t, root, "IMG_0005.jpg", content)
	}
	forceBufferedVerify(t)

	client := &scriptedClient{}
	e := newTestEngine(t, client, archiveRoot, localRoot)
	e.Ingest.RequireUnbuffered = true

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if fr.Err == nil || fr.Skipped {
		t.Fatalf("result = {Err:%v Skipped:%v}, want an error: the required unbuffered verify did not happen", fr.Err, fr.Skipped)
	}
	if len(client.posts) != 0 {
		t.Errorf("posts = %d, want 0", len(client.posts))
	}
	for _, root := range []string{archiveRoot, localRoot} {
		if got, err := os.ReadFile(filepath.Join(root, "IMG_0005.jpg")); err != nil || !bytes.Equal(got, content) {
			t.Errorf("%s: pre-existing copy changed or removed (err=%v)", root, err)
		}
	}
}

// Copies at the destination whose archive half differs from the local half
// are an error, never a registration and never deleted.
func TestIngestCardReingestRefusesMismatchedCopies(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	content := bytes.Repeat([]byte("x"), 64)
	writeCardFile(t, cardRoot, "IMG_0006.jpg", content)
	archiveRoot, localRoot := filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	writeCardFile(t, localRoot, "IMG_0006.jpg", content)
	writeCardFile(t, archiveRoot, "IMG_0006.jpg", content)

	// ResolveDestination's sampled match sees identical files; the
	// verify read of the archive copy is what differs (as a copy that
	// differs outside FastHash's samples, or a flaky NAS, would).
	client := &scriptedClient{}
	e := newTestEngine(t, client, archiveRoot, localRoot)
	origOpen := openForVerifyFn
	openForVerifyFn = func(path string) (io.ReadCloser, VerifyMethod, error) {
		if path == filepath.Join(archiveRoot, "IMG_0006.jpg") {
			return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("y"), 64))), VerifyMethodUnbuffered, nil
		}
		return origOpen(path)
	}
	t.Cleanup(func() { openForVerifyFn = origOpen })

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if fr.Err == nil || len(client.posts) != 0 {
		t.Fatalf("Err=%v posts=%d, want an error and no registration", fr.Err, len(client.posts))
	}
	for _, root := range []string{archiveRoot, localRoot} {
		if _, err := os.Stat(filepath.Join(root, "IMG_0006.jpg")); err != nil {
			t.Errorf("%s copy removed: %v", root, err)
		}
	}
}

// Cancelling while a file is being copied stops the copy within a buffer,
// removes the partial copies and processes no further files.
func TestIngestCardCancelStopsMidCopy(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB: many io.Copy buffers
	writeCardFile(t, cardRoot, "A.mov", big)
	writeCardFile(t, cardRoot, "B.mov", big)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &scriptedClient{}
	archiveRoot, localRoot := filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	e := newTestEngine(t, client, archiveRoot, localRoot)
	var maxSeen int64
	e.Progress = func(ev ProgressEvent) {
		if ev.BytesDone > maxSeen {
			maxSeen = ev.BytesDone
		}
		cancel() // after the first buffer of the first file
	}

	res, err := e.IngestCard(ctx, cardRoot)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("IngestCard err = %v, want context.Canceled", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("processed %d files, want only the one in flight when cancelled", len(res.Files))
	}
	if fr := res.Files[0]; !errors.Is(fr.Err, context.Canceled) {
		t.Errorf("in-flight file Err = %v, want context.Canceled", fr.Err)
	}
	if maxSeen >= int64(len(big)) {
		t.Errorf("copy ran to the end (%d bytes) after cancel", maxSeen)
	}
	if len(client.posts) != 0 {
		t.Errorf("posts = %d, want 0", len(client.posts))
	}
	for _, root := range []string{archiveRoot, localRoot} {
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Errorf("%s holds %d entries after a cancelled copy, want none", root, len(entries))
		}
	}
}

// A context cancelled before the walk reaches a file means no file is
// started at all.
func TestIngestCardCancelledBeforeStartIngestsNothing(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeCardFile(t, cardRoot, "A.jpg", []byte("a"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := &scriptedClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	res, err := e.IngestCard(ctx, cardRoot)
	if !errors.Is(err, context.Canceled) || len(res.Files) != 0 || len(client.posts) != 0 {
		t.Fatalf("err=%v files=%d posts=%d, want context.Canceled with nothing ingested", err, len(res.Files), len(client.posts))
	}
}

// Cancelled once the copies are verified: the registration intent is still
// recorded (queue writes ignore the cancellation), so Drain finishes it.
func TestIngestCardCancelDuringPostKeepsIntent(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeCardFile(t, cardRoot, "C.jpg", []byte("c-bytes"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &scriptedClient{}
	client.post = func(ctx context.Context, _ branchdam.NodeCreatedPayload, _ string) (*branchdam.EventResponse, error) {
		cancel()
		return nil, ctx.Err()
	}
	store := openStoreT(t)
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	e.Queue = store

	res, err := e.IngestCard(ctx, cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v (the walk had no further files to stop before)", err)
	}
	if fr := res.Files[0]; fr.Err != nil || !fr.Queued {
		t.Fatalf("Err=%v Queued=%v, want queued", fr.Err, fr.Queued)
	}
	if row := onlyRow(t, store); row.NodeCreatedStatus != queue.StatusPending || row.NodeCreatedAttempts != 1 {
		t.Errorf("row = %+v, want PENDING with the cancelled attempt recorded", row)
	}
}

// WithContext stops each copy/verify primitive at its next read.
func TestWriteAndVerifyHonourContext(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, bytes.Repeat([]byte("z"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := DualWrite(src, filepath.Join(dir, "a", "x"), filepath.Join(dir, "l", "x"), WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Errorf("DualWrite err = %v, want context.Canceled", err)
	}
	for _, p := range []string{filepath.Join(dir, "a", "x"), filepath.Join(dir, "l", "x")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("DualWrite left %s behind after cancel (err=%v)", p, err)
		}
	}
	if _, err := WriteLocal(src, filepath.Join(dir, "w"), WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Errorf("WriteLocal err = %v, want context.Canceled", err)
	}
	if _, err := Verify(src, "", WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Errorf("Verify err = %v, want context.Canceled", err)
	}
	if err := CopyToArchive(src, filepath.Join(dir, "arch", "x"), "h", WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Errorf("CopyToArchive err = %v, want context.Canceled", err)
	}
}

// Resuming an offline row whose local copy never reached its final name
// applies RequireUnbuffered to the verify of the recovered copy.
func TestCompleteResumedLocalCopyHonoursRequireUnbuffered(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "card", "D.jpg")
	writeCardFile(t, filepath.Dir(src), "D.jpg", []byte("d-bytes"))
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(dir, "local", "D.jpg")
	rec := queue.Record{
		LocalPath:         localPath,
		SizeBytes:         info.Size(),
		FullHash:          blake3Hex(t, src),
		ArchiveCopyStatus: queue.StatusPending,
	}
	forceBufferedVerify(t)
	e := &Engine{}
	e.Ingest.RequireUnbuffered = true
	if err := e.completeResumedLocalCopy(context.Background(), src, info, rec); err == nil {
		t.Fatal("expected an error: the recovered copy only had a buffered verify")
	}
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Errorf("copy placed at %s despite the failed verify requirement (err=%v)", localPath, err)
	}

	e.Ingest.RequireUnbuffered = false
	if err := e.completeResumedLocalCopy(context.Background(), src, info, rec); err != nil {
		t.Fatalf("without the requirement: %v", err)
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Errorf("copy not placed: %v", err)
	}
}

// With a queue, a fresh copy the server confirms as a live duplicate is
// removed before registration, so registerNode never runs: no queue row
// (a row would claim content that is not in the archive) and no POST.
func TestIngestCardDuplicateWritesNoQueueRow(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeCardFile(t, cardRoot, "DSC0001.JPG", []byte("duplicate-image-bytes"))

	client := &fakeCheckContentClient{checkFunc: prescreenServer(true, liveDuplicate("HIDDEN"))}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	store := openStoreT(t)
	e.Queue = store

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if fr.Err != nil || !fr.Skipped || fr.Queued || fr.ExistingNodeUUID == "" {
		t.Fatalf("result = {Err:%v Skipped:%v Queued:%v ExistingNodeUUID:%q}, want a duplicate skip", fr.Err, fr.Skipped, fr.Queued, fr.ExistingNodeUUID)
	}
	for _, p := range []string{fr.ArchivePath, fr.LocalPath} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: this run's copy of a duplicate must be removed (err=%v)", p, err)
		}
	}
	rows, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(client.calls) != 0 {
		t.Errorf("queue rows = %d, posts = %d, want 0 and 0 for a duplicate", len(rows), len(client.calls))
	}
}
