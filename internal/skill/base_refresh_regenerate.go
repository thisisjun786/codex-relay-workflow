package skill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The regenerate rule of a mechanical region (CRW-412): the file is derived and a command rebuilds it.
// The check does not read the file's content for a meaning. It checks the head out in a repository of
// its own, runs the command there twice and reads what changed: the command has to leave the head's
// files as the head has them, and to give the same result whatever the files it rebuilds held when it
// started. The command comes from the declaration, and it runs the code of the head under check with
// the rights of the caller, as running that head's tests would.

// treeEntry is one file of a tree.
type treeEntry struct{ mode, kind, oid string }

// treeEntries lists every file of a commit's tree by path.
func (g *refreshGit) treeEntries(ctx context.Context, commit string) (map[string]treeEntry, error) {
	_, out, err := g.iso(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	entries := map[string]treeEntry{}
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, name, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || name == "" {
			return nil, fmt.Errorf("git ls-tree wrote %q, which is not a tree entry", rec)
		}
		entries[name] = treeEntry{mode: fields[0], kind: fields[1], oid: fields[2]}
	}
	return entries, nil
}

// checkoutTree makes dir a repository of its own whose HEAD is commit and whose work tree is that
// commit's, with no configuration, hook or attribute but the commit's own: the throwaway repository's
// objects are borrowed through alternates, so nothing is written to the checkout under check.
func (g *refreshGit) checkoutTree(ctx context.Context, commit, dir string) error {
	if _, _, err := gitAt(ctx, g.isoEnv, "init", "-q", dir); err != nil {
		return err
	}
	alternates := filepath.Join(dir, ".git", "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alternates), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(alternates, []byte(g.objects+"\n"), 0o600); err != nil {
		return err
	}
	_, _, err := gitAt(ctx, g.isoEnv, "-C", dir, "checkout", "-q", "-f", "--detach", commit)
	return err
}

// snapshot is a hash of every file of dir outside .git, by path: a regular file by its bytes, a link
// by its target.
func snapshot(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".git" && d.IsDir():
			return filepath.SkipDir
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link:" + target
		case d.Type().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out[rel] = "file:" + hex.EncodeToString(sum[:])
		}
		return nil
	})
	return out, err
}

// regenFailure is a command that ran and failed.
type regenFailure struct{ status, tail string }

// limitedBuffer keeps the first bytes a command writes: enough to say why it failed.
type limitedBuffer struct{ buf bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 16384 - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// runCommand runs a command through sh in dir, in a process group of its own that a timeout ends as a
// whole, and with the caller's environment without the GIT_* variables that would point git at another
// repository. A command that ran and failed is a regenFailure; one that could not run, or ran out of
// time, is an error.
func runCommand(ctx context.Context, dir, command string, timeout time.Duration) (*regenFailure, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = dir
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("the regeneration command %q timed out after %s", command, timeout)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		lines := strings.Split(strings.TrimSpace(output.buf.String()), "\n")
		return &regenFailure{status: exit.String(), tail: strings.Join(lines[max(0, len(lines)-8):], "\n")}, nil
	}
	return nil, fmt.Errorf("the regeneration command %q could not run: %w", command, err)
}

// regenerationResult is what the two runs of one command showed: a refusal (code and detail), or none.
type regenerationResult struct{ code, detail string }

// regenerate runs one command twice on the head and compares. paths are the resolved paths the
// command's rule settles; scope says whether a path lies in the regions that name the command; tracked
// are the head's files; devEntries are the dev tip's, from which the second run starts: it begins with
// each of the resolved paths as the dev tip has it (removed when the dev tip has none), so a command
// that only keeps what it finds in the file cannot pass. Over the head's files and the files the
// regions cover, the two runs have to agree, and the first has to leave every file as the head has it;
// files the command creates elsewhere (build outputs) are not read.
func (g *refreshGit) regenerate(ctx context.Context, head, command string, paths []string, scope func(string) bool, tracked, devEntries map[string]treeEntry, timeout time.Duration) (*regenerationResult, error) {
	dirs := [2]string{}
	for i := range dirs {
		dir, err := os.MkdirTemp(g.dir, "run-")
		if err != nil {
			return nil, err
		}
		dirs[i] = dir
		if err := g.checkoutTree(ctx, head, dir); err != nil {
			return nil, err
		}
	}
	baseline, err := snapshot(dirs[0])
	if err != nil {
		return nil, err
	}
	if err := g.revertTo(ctx, dirs[1], paths, devEntries); err != nil {
		return nil, err
	}
	var finals [2]map[string]string
	for i, dir := range dirs {
		failure, err := runCommand(ctx, dir, command, timeout)
		if err != nil {
			return nil, err
		}
		if failure != nil {
			return &regenerationResult{"regeneration_failed", fmt.Sprintf("the regeneration command %q failed on the head (run %d of 2): %s: %s", command, i+1, failure.status, failure.tail)}, nil
		}
		if finals[i], err = snapshot(dir); err != nil {
			return nil, err
		}
	}
	touched := map[string]bool{}
	for p := range baseline {
		touched[p] = true
	}
	for _, final := range finals {
		for p := range final {
			touched[p] = true
		}
	}
	relevant := func(p string) bool { _, ok := tracked[p]; return ok || scope(p) }
	var differing, changedIn, changedOut []string
	for p := range touched {
		if !relevant(p) {
			continue
		}
		if finals[0][p] != finals[1][p] {
			differing = append(differing, p)
		}
		if finals[0][p] != baseline[p] {
			if scope(p) {
				changedIn = append(changedIn, p)
			} else {
				changedOut = append(changedOut, p)
			}
		}
	}
	switch {
	case len(differing) > 0:
		return &regenerationResult{"regeneration_not_deterministic", fmt.Sprintf("the two runs of %q gave different results at: %s (the second run started from the dev tip's version of the resolved files, so a result that depends on what the file held counts as different)", command, nameList(differing))}, nil
	case len(changedIn) > 0:
		return &regenerationResult{"regeneration_differs", fmt.Sprintf("the head's %s is not what %q makes of the head: the command changed it", nameList(changedIn), command)}, nil
	case len(changedOut) > 0:
		return &regenerationResult{"regeneration_touches_outside", fmt.Sprintf("%q changed %s, outside the regions that name it: the head does not carry that change, or the declaration does not describe the command", command, nameList(changedOut))}, nil
	}
	return nil, nil
}

// revertTo puts each path in dir as the dev tip has it, through a root that cannot leave dir; a path
// the dev tip does not have is removed.
func (g *refreshGit) revertTo(ctx context.Context, dir string, paths []string, devEntries map[string]treeEntry) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, p := range paths {
		entry, ok := devEntries[p]
		if !ok {
			if err := root.RemoveAll(p); err != nil {
				return err
			}
			continue
		}
		if entry.kind != "blob" || (entry.mode != "100644" && entry.mode != "100755") {
			return fmt.Errorf("%s is not a regular file in the dev tip (mode %s, %s)", p, entry.mode, entry.kind)
		}
		content, err := g.blob(ctx, entry.oid)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if entry.mode == "100755" {
			perm = 0o755
		}
		if err := root.WriteFile(p, []byte(content), perm); err != nil {
			return err
		}
	}
	return nil
}

// nameList names up to ten paths.
func nameList(paths []string) string {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	if len(sorted) > 10 {
		return strings.Join(sorted[:10], ", ") + fmt.Sprintf(" and %d more", len(sorted)-10)
	}
	return strings.Join(sorted, ", ")
}
