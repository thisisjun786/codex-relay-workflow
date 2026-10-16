package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workdirs"
)

func cleanupEligible(info RunInfo) bool {
	return !info.Live && (info.State == StateCompleted || info.State == StateCancelled) &&
		info.Unsettled == 0 && info.Holds == 0
}

type cleanupSkipped struct{ reason string }

func (e *cleanupSkipped) Error() string { return e.reason }

// openCleanupRoot anchors deletion below real flow/run directories. The selected state root may
// itself have a configured alias; aliases beneath that root are never used as containers.
func openCleanupRoot(state, run string) (*os.Root, error) {
	if !validRunID(run) {
		return nil, fmt.Errorf("%q is not a run id", run)
	}
	base, err := os.OpenRoot(state)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	flow, err := realChildRoot(base, "flow")
	if err != nil {
		return nil, err
	}
	defer flow.Close()
	return realChildRoot(flow, run)
}

func realChildRoot(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("%s is not a real directory", filepath.Join(parent.Name(), name))
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	held, err := child.Stat(".")
	if err != nil || !os.SameFile(before, held) {
		_ = child.Close()
		return nil, fmt.Errorf("%s changed while opening its directory", name)
	}
	return child, nil
}

func (c *command) cleanupWorktrees(args []string) int {
	flags, cm := c.flags()
	apply := flags.Bool("apply", false, "remove the worktrees shown by the preview")
	positional, code, ok := parse(flags, args)
	if !ok {
		return code
	}
	if len(positional) > 1 || (len(positional) == 1 && !validRunID(positional[0])) {
		return c.usage("takes at most one valid run id")
	}
	r, err := c.reader(cm)
	if err != nil {
		return c.fail("%v", err)
	}
	defer r.Close()
	var records []RunRecord
	if len(positional) == 1 {
		rec, err := r.Record(c.ctx, positional[0])
		if err != nil {
			return c.fail("%v", err)
		}
		records = []RunRecord{rec}
	} else if records, err = r.Records(c.ctx); err != nil {
		return c.fail("%v", err)
	}
	code = 0
	for _, rec := range records {
		if err := c.cleanupOne(r, rec, *apply); err != nil {
			var skip *cleanupSkipped
			if errors.As(err, &skip) {
				fmt.Fprintf(c.stdout, "kept run=%s reason=%q\n", rec.RunID, skip.reason)
				if len(positional) == 1 {
					code = 1
				}
				continue
			}
			fmt.Fprintf(c.stderr, "crw flow cleanup-worktrees: cleanup incomplete for run %s: %v\n", rec.RunID, err)
			code = 1
		}
	}
	return code
}

func (c *command) cleanupOne(r *Reader, rec RunRecord, apply bool) error {
	root, err := openCleanupRoot(r.State, rec.RunID)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(c.stdout, "run %s: no worktrees\n", rec.RunID)
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := r.Info(c.ctx, rec)
	if err != nil {
		return err
	}
	if !cleanupEligible(info) || LockHeld(r.State, rec.RunID) || startLockHeld(r.State, rec.RunID) {
		return &cleanupSkipped{reason: fmt.Sprintf("state=%s, unsettled=%d, holds=%d; only an idle completed or cancelled run can be cleaned",
			info.State, info.Unsettled, info.Holds)}
	}
	var lock *Lock
	if apply {
		lock, err = TakeLock(r.State, rec.RunID, "cleanup-worktrees", c.now())
		if err != nil {
			if errors.Is(err, ErrRunLive) {
				return &cleanupSkipped{reason: err.Error()}
			}
			return err
		}
		defer lock.Release()
		events, _, err := journal.LoadLean(c.ctx, r.St, rec.RunID)
		if err != nil {
			return err
		}
		if !cleanupEligible(Summarize(rec, events, false, 0)) || startLockHeld(r.State, rec.RunID) {
			return &cleanupSkipped{reason: "the run is no longer eligible under its lock"}
		}
	}
	roots, err := workdirs.Roots(root)
	if err != nil {
		return err
	}
	if !apply {
		for _, rel := range roots {
			size, err := cleanupBytes(root, rel)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "preview run=%s path=%q bytes=%d\n", rec.RunID, filepath.Join(root.Name(), rel), size)
		}
		if len(roots) == 0 {
			fmt.Fprintf(c.stdout, "preview run=%s: no worktrees\n", rec.RunID)
		}
		return nil
	}
	spec, err := cleanupSpec(root, rec.RunID)
	if err != nil {
		return err
	}
	captures, err := cleanupCapturePaths(c.ctx, r.St, rec.RunID)
	if err != nil {
		return err
	}
	return removeRunWorkdirs(c.ctx, root, spec, roots, captures, func(rel string) {
		fmt.Fprintf(c.stdout, "removed run=%s path=%q\n", rec.RunID, filepath.Join(root.Name(), rel))
	})
}

// cleanupFinishedUnderLock is called only by the run or cancel writer that still owns run.lock.
// It reads the durable end, with its own holder excluded from the liveness judgement.
func (c *command) cleanupFinishedUnderLock(state, socket, run string) {
	if c.ctx.Err() != nil {
		return
	}
	warn := func(err error) {
		fmt.Fprintf(c.stderr, "crw flow %s: warning: worktree cleanup incomplete for run %s: %v\n", c.name, run, err)
	}
	root, err := openCleanupRoot(state, run)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		warn(err)
		return
	}
	defer root.Close()
	roots, err := workdirs.Roots(root)
	if err != nil {
		warn(err)
		return
	}
	if len(roots) == 0 {
		return
	}
	r, err := c.reader(&common{state: state, socket: socket})
	if err != nil {
		warn(err)
		return
	}
	defer r.Close()
	rec, err := r.Record(c.ctx, run)
	if err != nil {
		warn(err)
		return
	}
	events, _, err := journal.LoadLean(c.ctx, r.St, run)
	if err != nil {
		warn(err)
		return
	}
	info := Summarize(rec, events, false, 0)
	if !cleanupEligible(info) {
		if info.State == StateCancelled {
			warn(fmt.Errorf("cancelled run still has %d unsettled calls and %d recovery holds", info.Unsettled, info.Holds))
		}
		return
	}
	spec, err := cleanupSpec(root, run)
	if err == nil {
		err = func() error {
			captures, qerr := cleanupCapturePaths(c.ctx, r.St, run)
			if qerr != nil {
				return qerr
			}
			return removeRunWorkdirs(c.ctx, root, spec, roots, captures, nil)
		}()
	}
	if err != nil {
		warn(err)
	}
}

func cleanupSpec(root *os.Root, run string) (Spec, error) {
	info, err := root.Lstat("run.json")
	if err != nil {
		return Spec{}, err
	}
	if !info.Mode().IsRegular() {
		return Spec{}, errors.New("run.json is not a regular record")
	}
	raw, err := root.ReadFile("run.json")
	if err != nil {
		return Spec{}, err
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return Spec{}, err
	}
	if spec.Schema != SpecSchema || spec.RunID != run {
		return Spec{}, errors.New("run.json does not identify this run")
	}
	return spec, nil
}

func cleanupBytes(root *os.Root, rel string) (int64, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return 0, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return 0, nil
	}
	var size int64
	err = fs.WalkDir(root.FS(), rel, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				size += info.Size()
			}
		}
		return nil
	})
	return size, err
}

type cleanupSnapshot struct {
	Path     string `json:"path"`
	Ref      string `json:"ref"`
	Commit   string `json:"commit"`
	Head     string `json:"head"`
	Snapshot string `json:"snapshot"`
}

// cleanupGitRoots stops at each repository root, so discovering worktrees does not scan caches.
// A standalone repository has its only object database inside the candidate and is kept.
func cleanupGitRoots(root *os.Root, rel string) ([]string, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, nil
	}
	var found []string
	err = fs.WalkDir(root.FS(), rel, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		info, err := root.Lstat(filepath.Join(path, ".git"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s holds a standalone repository or linked Git metadata; it is kept", path)
		}
		found = append(found, path)
		return fs.SkipDir
	})
	return found, err
}

func cleanupInside(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func removeRunWorkdirs(ctx context.Context, root *os.Root, spec Spec, roots, captures []string, removed func(string)) error {
	git := ops.GitFunc(agent.ExecGit("git"))
	realRun, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return err
	}
	held, err := root.Stat(".")
	if err != nil {
		return err
	}
	named, err := os.Stat(realRun)
	if err != nil || !os.SameFile(held, named) {
		return errors.New("the run directory changed")
	}
	gitRoots := map[string][]string{}
	var total int
	for _, rel := range roots {
		gitRoots[rel], err = cleanupGitRoots(root, rel)
		if err != nil {
			return err
		}
		total += len(gitRoots[rel])
	}
	var repo, common string
	if total > 0 {
		if spec.Repo == "" {
			return errors.New("the run records no repository for preserving its worktrees")
		}
		if repo, err = filepath.EvalSymlinks(spec.Repo); err != nil {
			return err
		}
		if common, err = git(ctx, repo, nil, "rev-parse", "--path-format=absolute", "--git-common-dir"); err != nil {
			return err
		}
		if common, err = filepath.EvalSymlinks(common); err != nil {
			return err
		}
		for _, rel := range roots {
			candidate := filepath.Join(realRun, rel)
			if cleanupInside(repo, candidate) || cleanupInside(common, candidate) {
				return fmt.Errorf("%s contains the owner repository or its objects; it is kept", rel)
			}
		}
		// Validate every worktree before deleting any candidate.
		for _, list := range gitRoots {
			for _, rel := range list {
				path := filepath.Join(realRun, rel)
				top, err := git(ctx, path, nil, "rev-parse", "--show-toplevel")
				if err != nil || filepath.Clean(top) != path {
					return fmt.Errorf("%s is not its own worktree: %v", rel, err)
				}
				got, err := git(ctx, path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
				if err != nil {
					return err
				}
				got, err = filepath.EvalSymlinks(got)
				if err != nil || got != common {
					return fmt.Errorf("%s belongs to another repository; it is kept", rel)
				}
				dir, err := git(ctx, path, nil, "rev-parse", "--absolute-git-dir")
				if err != nil {
					return err
				}
				dir, err = filepath.EvalSymlinks(dir)
				if err != nil || dir == common {
					return fmt.Errorf("%s is not a linked worktree; it is kept", rel)
				}
			}
		}
	}
	if err := archiveCleanupArtifacts(ctx, root, roots, captures); err != nil {
		return err
	}
	for _, rel := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if err := root.Remove(rel); err != nil {
				return err
			}
			if removed != nil {
				removed(rel)
			}
			continue
		}
		for _, worktree := range gitRoots[rel] {
			kept, err := preserveCleanup(ctx, git, repo, filepath.Join(realRun, worktree), spec.RunID, worktree)
			if err != nil {
				return err
			}
			if err := recordCleanup(root, kept); err != nil {
				return err
			}
		}
		knownGitFiles := make(map[string]bool, len(gitRoots[rel]))
		for _, worktree := range gitRoots[rel] {
			knownGitFiles[filepath.Join(worktree, ".git")] = true
		}
		// Read-only generated cache directories are owned by this root too. Never chmod a link.
		if err := fs.WalkDir(root.FS(), rel, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Name() == ".git" && (!knownGitFiles[path] || !d.Type().IsRegular()) {
				return fmt.Errorf("%s contains another repository whose objects were not preserved; it is kept", path)
			}
			if d.Name() == "objects" {
				parent := filepath.Dir(path)
				head, herr := root.Lstat(filepath.Join(parent, "HEAD"))
				refs, rerr := root.Lstat(filepath.Join(parent, "refs"))
				if herr == nil && rerr == nil && head.Mode().IsRegular() && refs.IsDir() {
					return fmt.Errorf("%s contains a bare repository; it is kept", parent)
				}
			}
			if d.IsDir() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				return root.Chmod(path, info.Mode().Perm()|0o700)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, worktree := range gitRoots[rel] {
			if _, err := git(ctx, repo, nil, "worktree", "remove", "--force", filepath.Join(realRun, worktree)); err != nil {
				return err
			}
		}
		if err := root.RemoveAll(rel); err != nil {
			return err
		}
		if removed != nil {
			removed(rel)
		}
	}
	return nil
}

func preserveCleanup(ctx context.Context, git ops.GitFunc, repo, path, run, rel string) (cleanupSnapshot, error) {
	out := cleanupSnapshot{Path: rel}
	var err error
	if out.Head, err = ops.Head(ctx, git, path); err != nil {
		return out, err
	}
	if out.Snapshot, err = ops.Snapshot(ctx, git, path); err != nil {
		return out, err
	}
	key := sha256.Sum256([]byte(rel + "\x00" + out.Head + "\x00" + out.Snapshot))
	out.Ref = fmt.Sprintf("refs/crw/flow/%s/cleanup/%x", run, key)
	if commit, err := git(ctx, repo, nil, "rev-parse", "--verify", "--quiet", out.Ref); err == nil {
		tree, terr := git(ctx, repo, nil, "rev-parse", commit+"^{tree}")
		parent, perr := git(ctx, repo, nil, "rev-parse", commit+"^")
		if terr != nil || perr != nil || tree != out.Snapshot || parent != out.Head {
			return out, fmt.Errorf("cleanup ref %s already holds another snapshot", out.Ref)
		}
		out.Commit = commit
		return out, nil
	}
	out.Commit, err = ops.PreserveTree(ctx, git, repo, out.Snapshot, out.Head, out.Ref, "preserved worktree of ended run "+run)
	return out, err
}

func recordCleanup(root *os.Root, kept cleanupSnapshot) error {
	return appendCleanupRecord(root, "cleanup.jsonl", kept)
}

func appendCleanupRecord(root *os.Root, name string, kept any) error {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular record", name)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return fmt.Errorf("%s has another hard link", name)
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func cleanupTerminal(res *engine.Result) bool {
	return res != nil && (res.Status == engine.RunCompleted || res.ErrorKind == engine.ErrKindCancelled)
}

// cleanupPath limits a recorded checkout to a disposable root of this run.
func cleanupPath(run, rel string) bool {
	if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".git" {
			return false
		}
	}
	for path := rel; path != "."; path = filepath.Dir(path) {
		if workdirs.IsRoot("flow/" + run + "/" + filepath.ToSlash(path)) {
			return true
		}
	}
	return false
}

func cleanupOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// cleanupParent returns an anchored real parent, or nil when a parent is absent
// in a preflight. It never uses a symlink as a directory.
func cleanupParent(root *os.Root, rel string, create bool) (*os.Root, error) {
	parent, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(rel)
	if dir == "." {
		return parent, nil
	}
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		child, err := realChildRoot(parent, part)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err = parent.Mkdir(part, 0o700); err == nil || errors.Is(err, fs.ErrExist) {
				child, err = realChildRoot(parent, part)
			}
		}
		_ = parent.Close()
		if errors.Is(err, fs.ErrNotExist) && !create {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		parent = child
	}
	return parent, nil
}

// restoreCleanedWorkdirs runs under the resume's run lock, after its journal,
// program and budget checks. Existing checkouts are never overwritten.
func restoreCleanedWorkdirs(ctx context.Context, state string, spec Spec, events []engine.Event) error {
	root, err := openCleanupRoot(state, spec.RunID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.OpenFile("cleanup.jsonl", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("cleanup.jsonl is not a regular record")
	}
	latest := map[string]cleanupSnapshot{}
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		var row cleanupSnapshot
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			return fmt.Errorf("cleanup.jsonl: %w", err)
		}
		if !cleanupPath(spec.RunID, row.Path) {
			return fmt.Errorf("invalid cleanup path %q", row.Path)
		}
		if !cleanupOID(row.Head) || !cleanupOID(row.Snapshot) || !cleanupOID(row.Commit) {
			return fmt.Errorf("invalid cleanup object IDs for %s", row.Path)
		}
		key := sha256.Sum256([]byte(row.Path + "\x00" + row.Head + "\x00" + row.Snapshot))
		if row.Ref != fmt.Sprintf("refs/crw/flow/%s/cleanup/%x", spec.RunID, key) {
			return fmt.Errorf("invalid cleanup ref for %s", row.Path)
		}
		latest[row.Path] = row
	}
	if err := scan.Err(); err != nil {
		return err
	}
	if len(latest) == 0 {
		return nil
	}
	if spec.Repo == "" {
		return errors.New("the run records no repository for restoring its worktrees")
	}
	var paths []string
	for path := range latest {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for i, path := range paths {
		for _, earlier := range paths[:i] {
			if cleanupInside(path, earlier) {
				return fmt.Errorf("cleanup paths %s and %s overlap", earlier, path)
			}
		}
	}
	git := ops.GitFunc(agent.ExecGit("git"))
	var missing []string
	// Validate every missing checkout and its saved objects before creating any.
	for _, path := range paths {
		parent, err := cleanupParent(root, path, false)
		if err != nil {
			return err
		}
		if parent != nil {
			info, err := parent.Lstat(filepath.Base(path))
			_ = parent.Close()
			if err == nil {
				if !info.IsDir() {
					return fmt.Errorf("%s is not a real worktree directory", path)
				}
				continue
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		row := latest[path]
		commit, err := git(ctx, spec.Repo, nil, "rev-parse", "--verify", "--quiet", row.Ref)
		if err != nil || commit != row.Commit {
			return fmt.Errorf("cleanup ref %s no longer holds its recorded commit: %v", row.Ref, err)
		}
		tree, terr := ops.TreeOf(ctx, git, spec.Repo, commit)
		base, berr := git(ctx, spec.Repo, nil, "rev-parse", commit+"^")
		if terr != nil || berr != nil || tree != row.Snapshot || base != row.Head {
			return fmt.Errorf("cleanup ref %s no longer proves its HEAD and snapshot", row.Ref)
		}
		missing = append(missing, path)
	}
	var artifacts []cleanupArtifact
	if len(missing) != 0 {
		var err error
		artifacts, err = loadCleanupArtifacts(root, spec.RunID)
		if err != nil {
			return err
		}
	}
	for _, path := range missing {
		if err := restoreCleanupCheckout(ctx, git, root, spec.Repo, latest[path], artifacts); err != nil {
			return fmt.Errorf("restore %s: %w", path, err)
		}
	}
	// Scheduler handles are derived from durable allocations, not from following
	// the old aliases. Only paths in this run's preserved checkout inventory qualify.
	nodes := map[string]bool{}
	for _, ev := range events {
		if node := str(ev.Data, "node"); node != "" {
			nodes[node] = true
		}
	}
	for node := range nodes {
		wt := worktreeOf(events, node)
		rel, err := filepath.Rel(root.Name(), wt)
		if err != nil || latest[rel].Path == "" {
			continue
		}
		alias := filepath.Join("node-wt", pathElement(node))
		if filepath.Join(root.Name(), alias) == wt {
			// An older layout may put the checkout directly at its scheduler handle.
			continue
		}
		parent, err := cleanupParent(root, alias, true)
		if err != nil {
			return err
		}
		name := filepath.Base(alias)
		if info, err := parent.Lstat(name); err == nil {
			target, linkErr := parent.Readlink(name)
			_ = parent.Close()
			if info.Mode()&fs.ModeSymlink == 0 || linkErr != nil || target != wt {
				return fmt.Errorf("the existing worktree handle %s is different", alias)
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			_ = parent.Close()
			return err
		}
		err = parent.Symlink(wt, name)
		_ = parent.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
