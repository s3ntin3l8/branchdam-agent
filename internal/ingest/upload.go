package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/netgate"
	"github.com/s3ntin3l8/branchdam-agent/internal/phash"
)

// uploadStream mode (ingest.uploadStream: true) sends each card file to
// POST /api/v1/agent/upload instead of writing an archive copy itself.
// Per file it:
//
//  1. reads the card exactly once, into a temp file under LocalEditRoot,
//     computing BLAKE3 and FastHash as the bytes pass (WriteLocal) --
//     invariant 1's "never re-read the card to hash";
//  2. cache-defeating-verifies that local copy (Verify);
//  3. uploads the verified local copy -- not the card -- with
//     X-Blake3-Hash set, so the server checks the bytes it stored against
//     the hash of what left the card, and can answer a known file with a
//     200 DEDUPLICATED without receiving it at all;
//  4. renames the temp file to LocalEditRoot/<server relativePath>, after
//     refusing any relativePath that is absolute or climbs out with "..".
//
// Sidecars (.xmp, .srt) are uploaded after every other file of the card,
// with their master's camera model, capture time and final stem, so the
// server's naming template renders them into the master's directory under
// the master's name -- its XMP and DJI-telemetry resolvers only pair
// files in the same directory with the same stem. (The server accepts any
// extension on this endpoint; what went wrong before was placement: a
// sidecar sent with its own metadata landed in an unknown_camera folder.)
//
// On a metered connection with ingest.pauseUploadOnMetered set, steps 3-4
// are skipped: the verified local copy is placed under the agent's own
// path template and the file is reported Deferred. Re-inserting the card
// finds that copy (identical bytes at the planned name) and reuses it
// instead of failing on the existing file. Deferred files are not queued
// in queue.db -- see docs/offline-queue.md's "Upload mode on a metered
// connection".

const deferredUploadReason = "upload skipped on metered connection -- re-insert card to retry"

// errUnsafeRelativePath is returned (wrapped) when the server's
// relativePath would place the local edit copy outside LocalEditRoot.
var errUnsafeRelativePath = errors.New("server relativePath is not a safe relative path")

// errLocalCopyConflict is returned (wrapped) when a different file already
// sits at the local edit copy's final name. Nothing is overwritten.
var errLocalCopyConflict = errors.New("local edit copy destination exists with different content")

// safeRelativePath turns the server's slash-separated relativePath into a
// native relative path that stays inside the root it is joined to. It
// refuses an empty or absolute path, a Windows drive or UNC prefix, a
// backslash (the server never emits one; on Windows it would be a
// separator the checks below don't see), NUL, and any ".." element --
// even one that path.Clean would fold away, since a server that sends
// one is not to be trusted with the rest of the path either.
func safeRelativePath(rel string) (string, error) {
	switch {
	case rel == "":
		return "", fmt.Errorf("%w: empty", errUnsafeRelativePath)
	case strings.ContainsAny(rel, "\\\x00"):
		return "", fmt.Errorf("%w: %q contains a backslash or NUL", errUnsafeRelativePath, rel)
	case strings.HasPrefix(rel, "/"), len(rel) >= 2 && rel[1] == ':':
		return "", fmt.Errorf("%w: %q is absolute", errUnsafeRelativePath, rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q contains \"..\"", errUnsafeRelativePath, rel)
		}
	}
	clean := path.Clean(rel)
	if clean == "." {
		return "", fmt.Errorf("%w: %q names no file", errUnsafeRelativePath, rel)
	}
	native := filepath.FromSlash(clean)
	if !filepath.IsLocal(native) {
		return "", fmt.Errorf("%w: %q", errUnsafeRelativePath, rel)
	}
	return native, nil
}

// uploadPass is one IngestCard call's upload-mode state: which masters
// were handled (so their sidecars can follow them) and the sidecars held
// back until every master has been.
type uploadPass struct {
	masters  map[string]masterRef
	sidecars []string
}

type masterRef struct {
	m *uploadedMaster
	// byName: the key was the master's full base name ("IMG_1.CR3", for
	// an "IMG_1.CR3.xmp" sidecar), not its stem.
	byName bool
}

// uploadedMaster is what a sidecar needs from its master.
type uploadedMaster struct {
	srcPath          string
	cameraModel      string
	captureTimestamp int64
	// vars is the master's own template input, so a sidecar renders the
	// same directory (camera model, capture date) as its master.
	vars TemplateVars
	// finalName is the base name the master ended up with: the server's
	// (possibly collision-suffixed) name, or the deferred local copy's.
	finalName string
	// localDir is the directory of the master's local edit copy.
	localDir string
	deferred bool
	failed   bool
}

func newUploadPass() *uploadPass {
	return &uploadPass{masters: make(map[string]masterRef)}
}

func (p *uploadPass) record(m *uploadedMaster) {
	dir, base := filepath.Split(m.srcPath)
	stem, _ := splitBase(base)
	// First master wins a shared stem (RAW+JPEG pairs): the walk is
	// lexical, so IMG_1.CR3 claims IMG_1.xmp before IMG_1.JPG does.
	if _, ok := p.masters[filepath.Join(dir, stem)]; !ok {
		p.masters[filepath.Join(dir, stem)] = masterRef{m: m}
	}
	p.masters[filepath.Join(dir, base)] = masterRef{m: m, byName: true}
}

// masterFor returns the master a sidecar belongs to, and the name the
// sidecar should carry so it follows the master's final name.
func (p *uploadPass) masterFor(sidecarPath string) (*uploadedMaster, string) {
	dir, base := filepath.Split(sidecarPath)
	stem, _ := splitBase(base)
	ref, ok := p.masters[filepath.Join(dir, stem)]
	if !ok {
		return nil, base
	}
	tail := base[len(stem):] // ".xmp", case preserved
	if ref.m.finalName == "" {
		return ref.m, base
	}
	if ref.byName {
		return ref.m, ref.m.finalName + tail
	}
	finalStem, _ := splitBase(ref.m.finalName)
	return ref.m, finalStem + tail
}

// uploadItem is one file's upload-mode input.
type uploadItem struct {
	srcPath          string
	info             os.FileInfo
	isSidecar        bool
	uploadName       string
	cameraModel      string
	captureTimestamp int64
	vars             TemplateVars
	// deferredLocalPath, when set, is the exact local path a deferred
	// copy goes to (a sidecar following its deferred master); otherwise
	// it is resolved from vars and the path template.
	deferredLocalPath string
	// forceDefer defers regardless of the metered probe (the master was
	// deferred, so the sidecar must not reach the server without it).
	forceDefer bool
}

// ingestFileUpload handles one non-sidecar file in uploadStream mode and
// records it in pass for its sidecars.
func (e *Engine) ingestFileUpload(ctx context.Context, srcPath string, pass *uploadPass) FileResult {
	fr := FileResult{SourcePath: srcPath}
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		fr.Err = fmt.Errorf("stat source: %w", err)
		pass.record(&uploadedMaster{srcPath: srcPath, failed: true})
		return fr
	}

	var exif *ExifResult
	if e.Exiftool != nil && e.Exiftool.HasExiftool() {
		if res, err := e.Exiftool.Exif(ctx, srcPath); err == nil {
			exif = res
		}
	}
	fr.Exif = exif

	captureTimestamp := srcInfo.ModTime().Unix()
	if exif != nil && exif.CapturedAt != nil {
		captureTimestamp = exif.CapturedAt.Unix()
	}
	var cameraModel string
	if exif != nil {
		cameraModel = exif.CameraModel
	}

	vars := buildTemplateVars(exif, srcPath, srcInfo.ModTime())
	fr = e.uploadFile(ctx, fr, uploadItem{
		srcPath:          srcPath,
		info:             srcInfo,
		uploadName:       filepath.Base(srcPath),
		cameraModel:      cameraModel,
		captureTimestamp: captureTimestamp,
		vars:             vars,
	})

	m := &uploadedMaster{
		srcPath:          srcPath,
		cameraModel:      cameraModel,
		captureTimestamp: captureTimestamp,
		vars:             vars,
		deferred:         fr.Deferred,
		failed:           fr.Err != nil,
	}
	if fr.LocalPath != "" {
		m.finalName = filepath.Base(fr.LocalPath)
		m.localDir = filepath.Dir(fr.LocalPath)
	}
	pass.record(m)
	return fr
}

// uploadSidecars handles the sidecars IngestCard held back, after every
// master of the card has been through ingestFileUpload. Like the walk, it
// checks ctx before each file: a cancelled ingest returns the sidecars
// already handled together with ctx's error.
func (e *Engine) uploadSidecars(ctx context.Context, pass *uploadPass) ([]FileResult, error) {
	out := make([]FileResult, 0, len(pass.sidecars))
	for _, srcPath := range pass.sidecars {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out = append(out, e.uploadSidecar(ctx, srcPath, pass))
	}
	return out, nil
}

func (e *Engine) uploadSidecar(ctx context.Context, srcPath string, pass *uploadPass) FileResult {
	fr := FileResult{SourcePath: srcPath, IsSidecar: true}
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		fr.Err = fmt.Errorf("stat source: %w", err)
		return fr
	}
	it := uploadItem{
		srcPath:          srcPath,
		info:             srcInfo,
		isSidecar:        true,
		uploadName:       filepath.Base(srcPath),
		captureTimestamp: srcInfo.ModTime().Unix(),
		vars:             buildTemplateVars(nil, srcPath, srcInfo.ModTime()),
	}

	master, name := pass.masterFor(srcPath)
	if master != nil {
		if master.failed {
			fr.Err = fmt.Errorf("sidecar held back: its master %s failed in this pass, and uploading the sidecar alone would detach it -- re-run the ingest", master.srcPath)
			return fr
		}
		it.uploadName = name
		it.cameraModel = master.cameraModel
		it.captureTimestamp = master.captureTimestamp
		// Render the master's directory, not an exif-less sidecar's
		// ("unknown_camera"): the temp copy is staged there, and the
		// deferred-copy lookup resolves there.
		it.vars.CameraModel = master.vars.CameraModel
		it.vars.CapturedAt = master.vars.CapturedAt
		if master.deferred {
			it.forceDefer = true
			if master.localDir != "" {
				it.deferredLocalPath = filepath.Join(master.localDir, name)
			}
		}
	}
	return e.uploadFile(ctx, fr, it)
}

// uploadFile runs the defer-or-upload decision for one prepared item.
func (e *Engine) uploadFile(ctx context.Context, fr FileResult, it uploadItem) FileResult {
	hold := it.forceDefer
	if !hold && e.Ingest.PauseUploadOnMetered {
		isMetered := e.IsMetered
		if isMetered == nil {
			isMetered = netgate.IsMetered
		}
		metered, mErr := isMetered()
		if mErr != nil {
			slog.Debug("metered probe failed, treating as metered (fail-closed)", "err", mErr)
		}
		hold = metered || mErr != nil
	}
	if hold {
		slog.Info("upload skipped on metered connection -- re-insert card to retry", "path", it.srcPath)
		return e.deferUpload(ctx, fr, it)
	}
	if e.Uploader == nil {
		fr.Err = fmt.Errorf("ingest: uploadStream is true but client does not support upload")
		return fr
	}
	return e.streamUpload(ctx, fr, it)
}

// stageLocalCopy reads the card once into tmpPath (removing a leftover of
// the agent's own from a killed run first -- the name marks it as ours)
// and cache-defeating-verifies it. On any error tmpPath is gone.
func (e *Engine) stageLocalCopy(ctx context.Context, fr *FileResult, it uploadItem, tmpPath string) error {
	_ = os.Remove(tmpPath)
	writeRes, err := WriteLocal(it.srcPath, tmpPath, e.progressOpts(ctx, it.srcPath, ProgressPhaseCopying, it.info.Size())...)
	if err != nil {
		return fmt.Errorf("write local copy: %w", err)
	}
	fr.Write = writeRes
	localVerify, err := Verify(tmpPath, writeRes.FullHash, e.progressOpts(ctx, it.srcPath, ProgressPhaseVerifying, writeRes.SizeBytes)...)
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("verify local copy: %w", err)
	}
	fr.LocalVerify = localVerify
	if !localVerify.Verified {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("ingest: verification failed for %s (local verified=false, want %s, got %s) -- safe-eject withheld", it.srcPath, writeRes.FullHash, localVerify.Hash)
	}
	if e.Ingest.RequireUnbuffered && localVerify.Method == VerifyMethodBufferedFloor {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("ingest: unbuffered verify required by config, but verify fell back to buffered floor (local=%s) -- safe-eject withheld", localVerify.Method)
	}
	return nil
}

// placeLocalCopy moves the verified temp copy to finalPath. A file
// already there that verifies against wantHash is the same bytes (an
// earlier deferred pass, or a re-run): the temp copy is dropped and the
// existing one kept. Anything else at finalPath is someone else's and is
// never touched. Reports whether tmpPath was the one placed.
func placeLocalCopy(ctx context.Context, tmpPath, finalPath, wantHash string) (placed bool, err error) {
	if existing, lerr := os.Lstat(finalPath); lerr == nil {
		if existing.Mode().IsRegular() {
			vr, verr := Verify(finalPath, wantHash, WithContext(ctx))
			if verr != nil {
				// Could not read it (I/O error, cancelled ingest): that
				// says nothing about whether it is ours, so it is not
				// reported as a conflict -- and still not touched.
				_ = os.Remove(tmpPath)
				return false, fmt.Errorf("compare with existing local edit copy %s: %w", finalPath, verr)
			}
			if vr.Verified {
				_ = os.Remove(tmpPath)
				return false, nil
			}
		}
		_ = os.Remove(tmpPath)
		return false, fmt.Errorf("%w: %s (want BLAKE3 %s); nothing was overwritten -- move the existing file away and re-run the ingest", errLocalCopyConflict, finalPath, wantHash)
	} else if !errors.Is(lerr, fs.ErrNotExist) {
		_ = os.Remove(tmpPath)
		return false, fmt.Errorf("stat local destination %s: %w", finalPath, lerr)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(finalPath), err)
	}
	if err := renameNoClobber(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return false, err
	}
	// renameNoClobber already fsynced the destination directory. When the
	// temp copy was staged elsewhere, its directory lost an entry too;
	// sync that one so the move is durable on both sides.
	if dir := filepath.Dir(tmpPath); dir != filepath.Dir(finalPath) {
		if dirErr := syncParentDirFn(dir); dirErr != nil {
			slog.Warn("upload: dir fsync failed; entry may not be durable on power loss", "dir", dir, "err", dirErr)
		}
	}
	return true, nil
}

// deferUpload keeps a verified local edit copy and reports the file
// Deferred: nothing reached the server, so the card stays the only copy
// outside this workstation.
func (e *Engine) deferUpload(ctx context.Context, fr FileResult, it uploadItem) FileResult {
	localPath := it.deferredLocalPath
	if localPath != "" && filesMatch(it.srcPath, localPath) {
		// A sidecar deferred with its master: an earlier deferred pass
		// already left these bytes beside it -- the master's own
		// AlreadyIngested check, applied at the master's directory.
		fr.LocalPath = localPath
		fr.Skipped = true
		fr.Deferred = true
		fr.SkipReason = deferredUploadReason
		return fr
	}
	if localPath == "" {
		res := ResolveDestination([]string{e.Ingest.LocalEditRoot}, e.Ingest.PathTemplate, it.vars, it.srcPath, "")
		localPath = filepath.Join(e.Ingest.LocalEditRoot, res.RelPath)
		if res.AlreadyIngested {
			// An earlier deferred pass already left these bytes here.
			fr.LocalPath = localPath
			fr.Skipped = true
			fr.Deferred = true
			fr.SkipReason = deferredUploadReason
			return fr
		}
	}
	fr.LocalPath = localPath

	tmpPath := agentTempName(localPath)
	if err := e.stageLocalCopy(ctx, &fr, it, tmpPath); err != nil {
		fr.Err = err
		return fr
	}
	placed, err := placeLocalCopy(ctx, tmpPath, localPath, fr.Write.FullHash)
	if err != nil {
		fr.Err = err
		return fr
	}
	if placed {
		preserveMtimeAt(it.srcPath, localPath, it.info.ModTime())
	}
	e.localPHash(ctx, &fr, localPath)

	fr.Skipped = true
	fr.Deferred = true
	fr.SkipReason = deferredUploadReason
	return fr
}

// streamUpload stages the local copy, uploads it with its hash, and
// places it at the server's relativePath.
func (e *Engine) streamUpload(ctx context.Context, fr FileResult, it uploadItem) FileResult {
	plannedLocal := filepath.Join(e.Ingest.LocalEditRoot, RenderPath(e.Ingest.PathTemplate, it.vars))
	tmpPath := agentTempName(plannedLocal)
	if err := e.stageLocalCopy(ctx, &fr, it, tmpPath); err != nil {
		fr.Err = err
		return fr
	}

	upResp, err := e.uploadLocalCopy(ctx, tmpPath, fr.Write, it)
	if err != nil {
		_ = os.Remove(tmpPath)
		fr.Err = fmt.Errorf("upload %s: %w", it.srcPath, err)
		return fr
	}
	fr.NodeUUID = upResp.NodeUUID
	fr.ArchivePath = upResp.RelativePath

	rel, err := safeRelativePath(upResp.RelativePath)
	if err != nil {
		_ = os.Remove(tmpPath)
		fr.Err = fmt.Errorf("place local copy of %s (node %s uploaded): %w", it.srcPath, upResp.NodeUUID, err)
		return fr
	}
	localPath := filepath.Join(e.Ingest.LocalEditRoot, rel)
	fr.LocalPath = localPath

	if moved := e.moveDeferredCopy(ctx, it, localPath, fr.Write.FullHash, fr.Write.SizeBytes); moved {
		_ = os.Remove(tmpPath)
		e.localPHash(ctx, &fr, localPath)
		return fr
	}

	placed, err := placeLocalCopy(ctx, tmpPath, localPath, fr.Write.FullHash)
	if err != nil {
		fr.Err = fmt.Errorf("place local copy of %s (node %s uploaded): %w", it.srcPath, upResp.NodeUUID, err)
		return fr
	}
	// Soft contract (issue #103): the archive landed server-side, so the
	// only Chtimes call on this path is the local edit copy. Failure is
	// logged, not fatal -- see preserveMtimeAt.
	if placed {
		preserveMtimeAt(it.srcPath, localPath, it.info.ModTime())
	}
	e.localPHash(ctx, &fr, localPath)
	return fr
}

// moveDeferredCopy handles a card re-inserted after a metered pass
// deferred this file: that pass left a verified local copy at the agent's
// own rendered name, and the server may now have placed the node
// elsewhere (a collision suffix, or a camera model it probed itself). The
// earlier copy is moved to the server's path so the edit tree holds one
// copy, not two -- but only when it holds exactly the uploaded bytes
// (full BLAKE3 against wantHash, so an edited copy is never moved) and
// nothing is at the server's path yet; a verified copy left behind is
// logged at WARN. Reports whether the earlier copy now sits at localPath.
//
// The candidates are the names a deferred pass can have used: the card's
// own base name (what deferUpload rendered) and, for a sidecar, the
// master-derived upload name -- each unsuffixed and with the collision
// suffixes (_2, _3, ...) ResolveDestination gives a copy whose name was
// taken. The sweep Lstats each render and stops after a run of
// maxDeferredSuffixGap absent suffixes (a hand-deleted _2 doesn't hide a
// live _3); only a regular file of the uploaded size is compared, against
// the hash already in hand, so the card is not read again. Each comparison
// is a full BLAKE3 of the local file, so the sweep is bounded by
// deferredSweepHashBudget bytes across all candidates: the first is always
// checked (one large video may exceed the budget on its own), and once a
// further check would overrun the budget the sweep stops with a WARN.
func (e *Engine) moveDeferredCopy(ctx context.Context, it uploadItem, localPath, wantHash string, wantSize int64) bool {
	names := []string{it.vars.OriginalName}
	if it.uploadName != "" && it.uploadName != it.vars.OriginalName {
		names = append(names, it.uploadName)
	}
	var hashed int64
	for _, name := range names {
		misses := 0
		for counter := 1; counter <= maxCollisionSuffix; counter++ {
			if ctx.Err() != nil {
				return false
			}
			suffix := ""
			if counter > 1 {
				suffix = fmt.Sprintf("_%d", counter)
			}
			vars := it.vars
			vars.OriginalName = SuffixedFilename(name, suffix)
			prior := filepath.Join(e.Ingest.LocalEditRoot, RenderPath(e.Ingest.PathTemplate, vars))
			if prior == localPath {
				continue // placeLocalCopy keeps an identical file there
			}
			info, err := os.Lstat(prior)
			if err != nil {
				// ResolveDestination takes suffixes in order, but a copy
				// can be deleted by hand afterwards (a missing _2 with a
				// live _3), so a short gap doesn't end the sweep; a run of
				// maxDeferredSuffixGap misses does.
				if counter > 1 {
					misses++
					if misses >= maxDeferredSuffixGap {
						break
					}
				}
				continue
			}
			misses = 0
			if !info.Mode().IsRegular() || info.Size() != wantSize {
				continue
			}
			if hashed > 0 && hashed+wantSize > deferredSweepHashBudget {
				slog.Warn("upload: stopped looking for an earlier deferred copy: hash budget spent; any copy not yet checked stays at its own name",
					"local", localPath, "hashedBytes", hashed, "budgetBytes", deferredSweepHashBudget, "next", prior)
				return false
			}
			hashed += wantSize
			if vr, err := Verify(prior, wantHash, WithContext(ctx)); err != nil || !vr.Verified {
				slog.Debug("upload: the file at the deferred name does not verify against the uploaded BLAKE3; leaving it in place",
					"path", prior, "err", err)
				continue
			}
			if _, err := os.Lstat(localPath); !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("upload: an earlier deferred copy stays at its own name; the server placed this file elsewhere",
					"deferred", prior, "local", localPath)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
				slog.Warn("upload: could not move the earlier deferred copy", "deferred", prior, "local", localPath, "err", err)
				return false
			}
			if err := renameNoClobber(prior, localPath); err != nil {
				slog.Warn("upload: could not move the earlier deferred copy", "deferred", prior, "local", localPath, "err", err)
				return false
			}
			if dir := filepath.Dir(prior); dir != filepath.Dir(localPath) {
				if dirErr := syncParentDirFn(dir); dirErr != nil {
					slog.Warn("upload: dir fsync failed; entry may not be durable on power loss", "dir", dir, "err", dirErr)
				}
			}
			slog.Info("upload: moved the earlier deferred copy to the server's path", "from", prior, "to", localPath)
			return true
		}
	}
	return false
}

// deferredSweepHashBudget caps the bytes moveDeferredCopy hashes (full
// BLAKE3 of each same-size candidate) for one file -- the same 2 GiB
// ResolveDestination allows its own collision sweep (hashBudget). A var
// so tests can shrink it.
var deferredSweepHashBudget = hashBudget

// maxDeferredSuffixGap is how many consecutive absent suffixes end
// moveDeferredCopy's sweep of one name.
const maxDeferredSuffixGap = 8

// uploadLocalCopy streams the verified local copy at p to the server.
func (e *Engine) uploadLocalCopy(ctx context.Context, p string, w WriteResult, it uploadItem) (*branchdam.UploadResponse, error) {
	f, err := os.Open(p) //nolint:gosec // p is the agent's own verified temp copy under LocalEditRoot
	if err != nil {
		return nil, fmt.Errorf("open local copy: %w", err)
	}
	defer func() { _ = f.Close() }()

	opts := branchdam.UploadOptions{
		Filename:         it.uploadName,
		CameraModel:      it.cameraModel,
		CaptureTimestamp: it.captureTimestamp,
		Blake3Hash:       w.FullHash,
		SourcePathHash:   sourcePathHash(it.srcPath),
		SizeBytes:        w.SizeBytes,
	}
	body := &progressReader{r: f}
	if o := applyWriteOptions(e.progressOpts(ctx, it.srcPath, ProgressPhaseUploading, w.SizeBytes)); o.onBytes != nil {
		body.onBytes = o.onBytes
	}
	resp, err := e.Uploader.Upload(ctx, body, opts)
	if err != nil {
		return nil, err
	}
	if resp.Blake3Hash != "" && !strings.EqualFold(resp.Blake3Hash, w.FullHash) {
		return nil, fmt.Errorf("server stored BLAKE3 %s for node %s at %s, local copy is %s -- safe-eject withheld; the server copy differs and should be removed",
			resp.Blake3Hash, resp.NodeUUID, resp.RelativePath, w.FullHash)
	}
	return resp, nil
}

// localPHash fills fr.PHash from the local edit copy (never the card).
func (e *Engine) localPHash(ctx context.Context, fr *FileResult, localPath string) {
	if fr.IsSidecar || e.Exiftool == nil || !isImageExt(extNoDot(localPath)) {
		return
	}
	if ph, err := phash.Extract(ctx, e.Exiftool.Pool(), localPath); err == nil {
		fr.PHash = ph
	}
}
