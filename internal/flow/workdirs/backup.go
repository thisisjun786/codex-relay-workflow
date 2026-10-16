package workdirs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Contains reports whether rel is a disposable root or anything below one.
func Contains(rel string) bool {
	for rel != "." {
		if IsRoot(rel) {
			return true
		}
		next := path.Dir(rel)
		if next == rel {
			break
		}
		rel = next
	}
	return false
}

type savedCheckout struct {
	Path, Ref, Commit, Head, Snapshot string
}

// validSavedCheckout keeps only a record the actual restorer would accept: a
// local path under a disposable root of this run, no .git part, hex object IDs
// and the ref its content key names.
func validSavedCheckout(run string, row savedCheckout) bool {
	if !filepath.IsLocal(row.Path) || filepath.Clean(row.Path) != row.Path ||
		!Contains("flow/"+run+"/"+filepath.ToSlash(row.Path)) {
		return false
	}
	for _, part := range strings.Split(row.Path, string(filepath.Separator)) {
		if part == ".git" {
			return false
		}
	}
	for _, id := range []string{row.Head, row.Snapshot, row.Commit} {
		if len(id) != 40 && len(id) != 64 {
			return false
		}
		if _, err := hex.DecodeString(id); err != nil {
			return false
		}
	}
	key := sha256.Sum256([]byte(row.Path + "\x00" + row.Head + "\x00" + row.Snapshot))
	return row.Ref == fmt.Sprintf("refs/crw/flow/%s/cleanup/%x", run, key)
}

// BackupSafe proves that the bytes being omitted are still reconstructable.
// No proof, dirty/untracked/ignored data, nested repositories or a failed probe
// means copying the worktree instead. Git probes never refresh the source index.
func BackupSafe(ctx context.Context, state, rel string) bool {
	if !IsRoot(rel) {
		return false
	}
	parts := strings.Split(rel, "/")
	run := parts[1]
	root, err := os.OpenRoot(filepath.Join(state, "flow", run))
	if err != nil {
		return false
	}
	defer root.Close()
	base := strings.Join(parts[2:], "/")
	var saved map[string]savedCheckout
	var ownerRepo string
	load := func() bool {
		if saved != nil {
			return true
		}
		specFile, err := root.OpenFile("run.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false
		}
		defer specFile.Close()
		specInfo, err := specFile.Stat()
		if err != nil || !specInfo.Mode().IsRegular() {
			return false
		}
		var spec struct {
			Schema string `json:"schema"`
			RunID  string `json:"run_id"`
			Repo   string `json:"repo"`
		}
		dec := json.NewDecoder(specFile)
		if dec.Decode(&spec) != nil {
			return false
		}
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			return false
		}
		// Unknown spec versions are copied until this reader understands their restore contract.
		if spec.Schema != "crw-flow-run/1" || spec.RunID != run || !filepath.IsAbs(spec.Repo) {
			return false
		}
		ownerRepo, err = filepath.EvalSymlinks(spec.Repo)
		if err != nil {
			return false
		}
		f, err := root.OpenFile("cleanup.jsonl", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
		saved = map[string]savedCheckout{}
		s := bufio.NewScanner(f)
		for s.Scan() {
			var row savedCheckout
			if json.Unmarshal(s.Bytes(), &row) != nil || !validSavedCheckout(run, row) {
				return false
			}
			saved[row.Path] = row
		}
		if s.Err() != nil {
			return false
		}
		// The whole valid record set is checked, as the restorer does: overlapping
		// roots mean the restorer would refuse, so this tree must be copied.
		for p := range saved {
			for parent := filepath.Dir(p); parent != "."; parent = filepath.Dir(parent) {
				if _, overlap := saved[parent]; overlap {
					return false
				}
			}
		}
		return true
	}
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "-c", "core.fsmonitor=false", "-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	proven := func(rel string) bool {
		if !load() {
			return false
		}
		row, ok := saved[rel]
		if !ok {
			return false
		}
		dir := filepath.Join(root.Name(), rel)
		commit, err := git(dir, "show-ref", "--verify", "--hash", row.Ref)
		if err != nil || commit != row.Commit {
			return false
		}
		head, err := git(dir, "rev-parse", "HEAD")
		if err != nil || head != row.Head {
			return false
		}
		tree, err := git(dir, "rev-parse", "HEAD^{tree}")
		if err != nil || tree != row.Snapshot {
			return false
		}
		parent, err := git(dir, "rev-parse", commit+"^")
		if err != nil || parent != head {
			return false
		}
		keptTree, err := git(dir, "rev-parse", commit+"^{tree}")
		if err != nil || keptTree != tree {
			return false
		}
		common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return false
		}
		common, err = filepath.EvalSymlinks(common)
		if err != nil {
			return false
		}
		realState, err := filepath.EvalSymlinks(state)
		if err != nil {
			return false
		}
		within, err := filepath.Rel(realState, common)
		if err != nil || Contains(filepath.ToSlash(within)) {
			return false
		}
		ownerWithin, err := filepath.Rel(realState, ownerRepo)
		if err != nil || Contains(filepath.ToSlash(ownerWithin)) {
			return false
		}
		ownerCommon, err := git(ownerRepo, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return false
		}
		ownerCommon, err = filepath.EvalSymlinks(ownerCommon)
		if err != nil || ownerCommon != common {
			return false
		}
		index, err := git(dir, "ls-files", "-v", "-z")
		if err != nil {
			return false
		}
		for _, entry := range strings.Split(index, "\x00") {
			// Lowercase marks assume-unchanged; S marks skip-worktree. Status can
			// hide real bytes in either case, so require ordinary tracked entries.
			if entry != "" && !strings.HasPrefix(entry, "H ") {
				return false
			}
		}
		status, err := git(dir, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
		if err != nil || status != "" {
			return false
		}
		ignored, err := git(dir, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory")
		return err == nil && ignored == ""
	}
	var checkouts []string
	err = fs.WalkDir(root.FS(), base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			info, err := root.Lstat(path.Join(p, ".git"))
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || !proven(p) {
				return errors.New("checkout is not preserved")
			}
			checkouts = append(checkouts, p)
			return nil // do not skip: a nested repository must be proved separately
		}
		if d.Type().IsRegular() {
			for _, checkout := range checkouts {
				if strings.HasPrefix(p, checkout+"/") {
					return nil
				}
			}
		}
		return errors.New("working data has no recovery proof")
	})
	return err == nil
}
