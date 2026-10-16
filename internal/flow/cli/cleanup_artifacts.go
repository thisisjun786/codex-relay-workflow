package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// cleanupCapturePaths lists the absolute artifact paths that completed crw.qaCapture results of the
// run recorded, so cleanup can keep their bytes with the run's records before removing the worktree
// that holds them. It runs under the run lock, after eligibility; the preview never runs it.
func cleanupCapturePaths(ctx context.Context, st *store.Store, run string) ([]string, error) {
	rows, err := st.DB.QueryContext(ctx, `SELECT call_id, result_bytes FROM flow_calls WHERE run_id = ? AND kind = ? AND event = ? ORDER BY event_seq`, run, "crw.qaCapture", engine.EvCompleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var call string
		var raw []byte
		if err := rows.Scan(&call, &raw); err != nil {
			return nil, err
		}
		var view ops.QACaptureView
		if err := json.Unmarshal(raw, &view); err != nil {
			return nil, fmt.Errorf("QA capture %s: %w", call, err)
		}
		for _, entry := range view.Captures {
			p := entry.Abs
			if p == "" && view.Root != "" {
				p = filepath.Join(view.Root, entry.Path)
			}
			if !filepath.IsAbs(p) {
				return nil, fmt.Errorf("QA capture %s has no absolute artifact path", call)
			}
			paths = append(paths, p)
		}
	}
	return paths, rows.Err()
}

type cleanupArtifact struct {
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath"`
	Blob         string `json:"blob"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Mode         uint32 `json:"mode"`
}

func openCleanupRegular(root *os.Root, path string) (*os.File, os.FileInfo, error) {
	parent, err := cleanupParent(root, path, false)
	if err != nil {
		return nil, nil, err
	}
	if parent == nil {
		return nil, nil, fs.ErrNotExist
	}
	defer parent.Close()
	f, err := parent.OpenFile(filepath.Base(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular artifact", path)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

func checkCleanupBlob(root *os.Root, a cleanupArtifact) error {
	f, _, err := openCleanupRegular(root, a.Blob)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return err
	}
	if n != a.Size || hex.EncodeToString(sum.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("saved artifact %s no longer matches its digest", a.Path)
	}
	return nil
}

// archiveCleanupArtifacts keeps evidence independently of Git's ignore rules.
// OriginalPath maps the immutable journal reference to the durable blob.
func archiveCleanupArtifacts(ctx context.Context, root *os.Root, roots, captures []string) error {
	realRun, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return err
	}
	owned := func(rel string) bool {
		for _, candidate := range roots {
			if cleanupInside(rel, candidate) {
				return true
			}
		}
		return false
	}
	paths := map[string]map[string]bool{}
	add := func(rel, original string) error {
		if !utf8.ValidString(rel) || !cleanupPath(filepath.Base(root.Name()), rel) {
			return fmt.Errorf("invalid artifact path %q", rel)
		}
		if paths[rel] == nil {
			paths[rel] = map[string]bool{}
		}
		paths[rel][original] = true
		return nil
	}
	for _, candidate := range roots {
		info, err := root.Lstat(candidate)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if err := fs.WalkDir(root.FS(), candidate, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			slash := "/" + filepath.ToSlash(p)
			evidence := strings.Contains(slash, "/.crw/evidence/")
			if d.Type()&fs.ModeSymlink != 0 && (evidence || strings.HasSuffix(slash, "/.crw") || strings.HasSuffix(slash, "/.crw/evidence")) {
				return fmt.Errorf("evidence path %s is a link; the worktree is kept", p)
			}
			if evidence && !d.IsDir() {
				if !d.Type().IsRegular() {
					return fmt.Errorf("evidence path %s is not a regular file", p)
				}
				return add(p, filepath.Join(realRun, p))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, original := range captures {
		// Resolve a recorded reference only to locate an owned file. No bytes
		// outside this run are read; ordinary tree enumeration never follows links.
		resolved, err := filepath.EvalSymlinks(original)
		if err != nil {
			rel, rerr := filepath.Rel(root.Name(), original)
			if rerr == nil && owned(rel) {
				return fmt.Errorf("cannot preserve QA capture %s: %w", original, err)
			}
			continue // outside cleanup's scope, and already unavailable
		}
		rel, err := filepath.Rel(realRun, resolved)
		if err != nil || !owned(rel) {
			continue
		}
		// A state/run directory alias stays after cleanup; a link beneath a
		// disposable root would disappear. Keep such a tree rather than lose
		// the journal's alias reference.
		suffix := string(filepath.Separator) + rel
		if !strings.HasSuffix(original, suffix) {
			return fmt.Errorf("QA capture %s uses a worktree alias; the worktree is kept", original)
		}
		originalRoot, err := filepath.EvalSymlinks(strings.TrimSuffix(original, suffix))
		if err != nil || filepath.Clean(originalRoot) != filepath.Clean(realRun) {
			return fmt.Errorf("QA capture %s uses a worktree alias; the worktree is kept", original)
		}
		if err := add(rel, original); err != nil {
			return err
		}
	}
	if len(paths) == 0 {
		return nil
	}
	if err := root.Mkdir("cleanup-artifacts", 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	blobs, err := realChildRoot(root, "cleanup-artifacts")
	if err != nil {
		return err
	}
	defer blobs.Close()
	var names []string
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		a, err := saveCleanupArtifact(root, blobs, p)
		if err != nil {
			return err
		}
		var originals []string
		for original := range paths[p] {
			originals = append(originals, original)
		}
		sort.Strings(originals)
		for _, original := range originals {
			a.OriginalPath = original
			if err := appendCleanupRecord(root, "cleanup-artifacts.jsonl", a); err != nil {
				return err
			}
		}
	}
	return nil
}

func saveCleanupArtifact(root, blobs *os.Root, path string) (cleanupArtifact, error) {
	a := cleanupArtifact{Path: path}
	src, before, err := openCleanupRegular(root, path)
	if err != nil {
		return a, err
	}
	defer src.Close()
	temp := ".tmp-" + rand.Text()
	dst, err := blobs.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return a, err
	}
	defer dst.Close()
	defer blobs.Remove(temp)
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, sum), src)
	if err != nil {
		return a, err
	}
	after, err := src.Stat()
	if err != nil {
		return a, err
	}
	named, err := root.Lstat(path)
	if err != nil || !named.Mode().IsRegular() || !os.SameFile(before, named) ||
		n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return a, fmt.Errorf("QA artifact %s changed while being preserved", path)
	}
	if err := dst.Sync(); err != nil {
		return a, err
	}
	if err := dst.Close(); err != nil {
		return a, err
	}
	a.SHA256, a.Size, a.Mode = hex.EncodeToString(sum.Sum(nil)), n, uint32(before.Mode().Perm())
	a.Blob = filepath.Join("cleanup-artifacts", a.SHA256)
	if err := blobs.Link(temp, a.SHA256); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return a, err
		}
		if err := checkCleanupBlob(root, a); err != nil {
			return a, err
		}
	}
	if err := blobs.Remove(temp); err != nil {
		return a, err
	}
	dir, err := blobs.Open(".")
	if err != nil {
		return a, err
	}
	defer dir.Close()
	return a, dir.Sync()
}

func loadCleanupArtifacts(root *os.Root, run string) ([]cleanupArtifact, error) {
	f, _, err := openCleanupRegular(root, "cleanup-artifacts.jsonl")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	latest := map[string]cleanupArtifact{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		var a cleanupArtifact
		if err := json.Unmarshal(s.Bytes(), &a); err != nil {
			return nil, err
		}
		if !cleanupPath(run, a.Path) || len(a.SHA256) != 64 || !cleanupOID(a.SHA256) ||
			a.Blob != filepath.Join("cleanup-artifacts", a.SHA256) || a.Size < 0 || a.Mode&^uint32(0o777) != 0 {
			return nil, fmt.Errorf("invalid saved artifact %q", a.Path)
		}
		latest[a.Path] = a
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	var out []cleanupArtifact
	for _, a := range latest {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// restoreCleanupCheckout publishes a fully reconstructed tree, including ignored
// QA evidence, at the recorded path. A failed staging copy is never mistaken for
// an existing restored checkout by a later resume.
func restoreCleanupCheckout(ctx context.Context, git ops.GitFunc, root *os.Root, repo string, row cleanupSnapshot, artifacts []cleanupArtifact) (err error) {
	for _, a := range artifacts {
		if cleanupInside(a.Path, row.Path) {
			if err := checkCleanupBlob(root, a); err != nil {
				return err
			}
		}
	}
	temp := ".cleanup-restore-" + rand.Text()
	if err := root.Mkdir(temp, 0o700); err != nil {
		return err
	}
	stageRel := filepath.Join(temp, "wt")
	stage := filepath.Join(root.Name(), stageRel)
	defer func() {
		if _, statErr := root.Lstat(stageRel); statErr == nil {
			_, removeErr := git(ctx, repo, nil, "worktree", "remove", "--force", stage)
			err = errors.Join(err, removeErr)
		}
		err = errors.Join(err, root.RemoveAll(temp))
	}()
	if _, err := ops.Restore(ctx, git, repo, stage, row.Head, row.Snapshot); err != nil {
		return err
	}
	for _, a := range artifacts {
		if !cleanupInside(a.Path, row.Path) {
			continue
		}
		suffix, err := filepath.Rel(row.Path, a.Path)
		if err != nil {
			return err
		}
		target := filepath.Join(stageRel, suffix)
		if err := restoreCleanupArtifact(root, target, a); err != nil {
			return err
		}
	}
	parent, err := cleanupParent(root, row.Path, true)
	if err != nil {
		return err
	}
	held, herr := parent.Stat(".")
	named, nerr := os.Stat(filepath.Join(root.Name(), filepath.Dir(row.Path)))
	_ = parent.Close()
	if herr != nil || nerr != nil || !os.SameFile(held, named) {
		return fmt.Errorf("the directory containing restore target %s changed", row.Path)
	}
	_, err = git(ctx, repo, nil, "worktree", "move", stage, filepath.Join(root.Name(), row.Path))
	return err
}

func restoreCleanupArtifact(root *os.Root, target string, a cleanupArtifact) error {
	if existing, info, err := openCleanupRegular(root, target); err == nil {
		sum := sha256.New()
		n, readErr := io.Copy(sum, existing)
		_ = existing.Close()
		if readErr != nil {
			return readErr
		}
		if n != a.Size || hex.EncodeToString(sum.Sum(nil)) != a.SHA256 {
			return fmt.Errorf("saved Git content and QA artifact %s disagree", a.Path)
		}
		if info.Mode().Perm() != fs.FileMode(a.Mode) {
			return root.Chmod(target, fs.FileMode(a.Mode))
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	src, _, err := openCleanupRegular(root, a.Blob)
	if err != nil {
		return err
	}
	defer src.Close()
	parent, err := cleanupParent(root, target, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	dst, err := parent.OpenFile(filepath.Base(target), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, sum), src)
	if err != nil {
		return err
	}
	if n != a.Size || hex.EncodeToString(sum.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("saved QA artifact %s changed", a.Path)
	}
	if err := dst.Chmod(fs.FileMode(a.Mode)); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	return dst.Close()
}
