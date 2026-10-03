// Package session is the session source binding: the Go form of CXC v0.2.40 pabcd-state/src/session-source.ts,
// session-source-identity.ts and source-gate.ts (commit 3c1459ac), ported as they are, edge cases included. A session
// keeps its state in the directory it started in (its native cwd) while its source work happens in a linked git
// worktree; the binding file .crw/sources/<session>.json pins that worktree, Capture takes the source identity there and
// CheckBound refuses a cycle whose source identity cannot be resolved.
//
// It is a subpackage of source because state imports source, and Resolve reads the session state. Git runs as a child
// process with the routing variables removed. Errors the oracle throws as messages are returned with that text; an
// operating-system failure the oracle lets through is returned as Go reports it, in Go's words.
//
// Not literal: where Node decodes git's output as text, so that the bytes of a path that are not UTF-8 become U+FFFD and
// name nothing, gitIdentity refuses output that is not valid UTF-8, which is the same refusal.
package session

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// refusal is a message the oracle throws, kept apart from the operating-system errors it lets through.
type refusal string

func (r refusal) Error() string { return string(r) }

// SourcesSubdir is the directory of the binding files under the state directory.
const SourcesSubdir = "sources"

// Binding is the immutable record of a session's source worktree, as .crw/sources/<session>.json stores it; the JSON
// names and order are the oracle's.
type Binding struct {
	Version        int    `json:"version"`
	OwnerSessionID string `json:"ownerSessionId"`
	NativeCwd      string `json:"nativeCwd"`
	SourceRoot     string `json:"sourceRoot"`
	CommonDir      string `json:"commonDir"`
	GitDir         string `json:"gitDir"`
}

// worktree is what git says about a worktree: its root, the repository's common directory and its own git directory,
// all canonical.
type worktree struct{ root, commonDir, gitDir string }

// canonical is the absolute path with every symlink resolved (realpathSync.native), or an error when it does not exist. The
// input is not cleaned first: ".." applies to the path the symlinks before it lead to, and a missing component fails.
func canonical(path string) (string, error) {
	if path == "" {
		return "", &fs.PathError{Op: "realpath", Path: path, Err: fs.ErrNotExist}
	}
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd + string(filepath.Separator) + path
	}
	return filepath.EvalSymlinks(path)
}

// gitProbeEnv is the environment without the routing variables, so a stray GIT_DIR cannot redirect a probe. These four and no
// more: the source identity removes two more, but a GIT_OBJECT_DIRECTORY that does not exist makes git take the directory for
// no repository, and the oracle's probes fail with it.
func gitProbeEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains([]string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"}, name)
	})
}

// probeOutputLimit is the default maxBuffer of execFileSync: a probe whose stdout and stderr together pass it fails.
const probeOutputLimit = 1 << 20

// git runs git in cwd and returns its trimmed stdout; a failure to start, a non-zero exit, a signal and too much output are all
// one error.
func git(cwd string, args ...string) (string, error) {
	out, err := source.Run(cwd, gitProbeEnv(), probeOutputLimit, "git", args...)
	return text.Trim(string(out)), err
}

// gitIdentity is the worktree identity of cwd, or an error: a repository is mandatory on the source side.
func gitIdentity(cwd string) (worktree, error) {
	var w worktree
	for _, probe := range []struct {
		into *string
		args []string
	}{
		{&w.root, []string{"rev-parse", "--show-toplevel"}},
		{&w.commonDir, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}},
		{&w.gitDir, []string{"rev-parse", "--absolute-git-dir"}},
	} {
		out, err := git(cwd, probe.args...)
		if err == nil && !utf8.ValidString(out) {
			// Node decodes the output as text, so the path it gets for such a name does not exist.
			err = fs.ErrNotExist
		}
		if err == nil {
			*probe.into, err = canonical(out)
		}
		if err != nil {
			return worktree{}, refusal("Cannot resolve source Git worktree identity.")
		}
	}
	return w, nil
}

// nativeGitIdentity is the identity of the native cwd (the state directory need not be a repository), with isRepo false when
// git rev-parse --git-dir fails, for any reason: the oracle takes that failure for "not inside a repository". When it succeeds,
// the first error is returned, so a repository whose identity cannot be resolved (a bare one) is not taken for no repository.
func nativeGitIdentity(cwd string) (w worktree, isRepo bool, err error) {
	if w, err = gitIdentity(cwd); err == nil {
		return w, true, nil
	}
	if _, probeErr := git(cwd, "rev-parse", "--git-dir"); probeErr != nil {
		return worktree{}, false, nil
	}
	return worktree{}, true, err
}

// bindingPath is the binding file of a session below cwd, refusing a state directory or sources directory that is not a
// real directory.
func bindingPath(cwd, sessionID string) (string, error) {
	if !state.IsCanonicalSessionID(sessionID) {
		return "", refusal("Invalid source-binding session ID.")
	}
	for _, dir := range []string{filepath.Join(cwd, crwdir.DirName), filepath.Join(cwd, crwdir.DirName, SourcesSubdir)} {
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", refusal("Source-binding directories must be real directories.")
		}
	}
	return filepath.Join(cwd, crwdir.DirName, SourcesSubdir, sessionID+".json"), nil
}

// readBinding is the session's binding, nil when it has none. A file that is not a regular file (a symlink is not
// followed), cannot be parsed or does not describe this session and native cwd is an error, and its bytes stay.
func readBinding(cwd, sessionID string) (*Binding, error) {
	path, err := bindingPath(cwd, sessionID)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, refusal("Source binding must be a regular file.")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	corrupt := refusal("Source binding is unreadable or corrupt; existing bytes were preserved.")
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		return nil, corrupt
	}
	data, err := io.ReadAll(f)
	var raw any
	if err != nil || json.Unmarshal(data, &raw) != nil {
		return nil, corrupt
	}
	b, ok := raw.(map[string]any)
	if !ok {
		return nil, refusal("Invalid source binding.")
	}
	invalid := refusal("Source binding has invalid identity or paths.")
	if version, _ := b["version"].(float64); version != 1 || b["ownerSessionId"] != sessionID {
		return nil, invalid
	}
	native, err := canonical(cwd)
	if err != nil {
		return nil, err
	}
	paths := [3]string{}
	for i, key := range []string{"sourceRoot", "commonDir", "gitDir"} {
		if paths[i], ok = b[key].(string); !ok || !filepath.IsAbs(paths[i]) {
			return nil, invalid
		}
	}
	if b["nativeCwd"] != native {
		return nil, invalid
	}
	return &Binding{Version: 1, OwnerSessionID: sessionID, NativeCwd: native, SourceRoot: paths[0], CommonDir: paths[1], GitDir: paths[2]}, nil
}

// Resolve is the root the session's source work happens in: its binding's worktree, or cwd itself when it has none (the
// legacy behaviour). A broken binding never falls back to cwd: it is an error, and so is a missing binding for a worktree
// the session's state pinned, or a source worktree that moved or whose repository identity changed.
func Resolve(cwd, sessionID string) (string, error) {
	binding, err := readBinding(cwd, sessionID)
	if err != nil {
		return "", err
	}
	pinned := ""
	if p := state.ReadState(cwd, sessionID).BoundSourceRoot; p != nil {
		pinned = *p
	}
	if binding == nil {
		if pinned != "" {
			return "", refusal("Source binding is missing for the pinned worktree; restore the same binding before continuing.")
		}
		return cwd, nil
	}
	if pinned != "" && binding.SourceRoot != pinned {
		return "", refusal("Source binding differs from the session's pinned worktree.")
	}
	native, isRepo, err := nativeGitIdentity(cwd)
	if err != nil {
		return "", err
	}
	source, err := gitIdentity(binding.SourceRoot)
	if err != nil {
		return "", err
	}
	// The three source clauses detect a moved or re-pointed worktree and are unconditional; only the native clause is
	// conditional, because a native cwd outside any repository has no common directory to compare (#109).
	if source.root != binding.SourceRoot || source.commonDir != binding.CommonDir || source.gitDir != binding.GitDir ||
		(isRepo && native.commonDir != binding.CommonDir) {
		return "", refusal("Bound source worktree moved or its repository identity changed.")
	}
	return binding.SourceRoot, nil
}

// Bind pins the session to the git worktree at target (an absolute path) and returns its root. The caller must corroborate
// the native identity and inspect the existing state first. In a git native cwd the target must be a linked worktree of the
// same repository; in one outside any repository it must be a repository root. Binding is immutable: repeating it for the
// same root is a no-op, another root is refused, and so is a session past A that has no pinned root. The file is published
// by hard link, so a binder that loses a race leaves the winner's file as it is.
func Bind(cwd, sessionID, target string) (string, error) {
	if !filepath.IsAbs(target) {
		return "", refusal("Source worktree path must be absolute.")
	}
	nativeCwd, err := canonical(cwd)
	if err != nil {
		return "", err
	}
	sourceRoot, err := canonical(target)
	if err != nil {
		return "", err
	}
	native, isRepo, err := nativeGitIdentity(cwd)
	if err != nil {
		return "", err
	}
	source, err := gitIdentity(sourceRoot)
	if err != nil {
		return "", err
	}
	if source.root != sourceRoot {
		return "", refusal("Source must be the root of a Git repository or worktree.")
	}
	if isRepo {
		if source.commonDir != native.commonDir || source.gitDir == native.gitDir {
			return "", refusal("Source must be a linked worktree root in the native session's repository.")
		}
	} else if nativeCwd == sourceRoot || strings.HasPrefix(nativeCwd, sourceRoot+string(filepath.Separator)) {
		// #109: the oracle calls this a defensive invariant that cannot fire, but it does when git cannot see the repository
		// from the native cwd (GIT_CEILING_DIRECTORIES). An ancestor binding would sweep the session's siblings into the
		// certified tree, so the check stays.
		return "", refusal("Source root must not contain the session's own working directory; bind the repository itself, not an ancestor of it.")
	}
	previous, err := readBinding(cwd, sessionID)
	if err != nil {
		return "", err
	}
	if previous != nil {
		if resolved, err := Resolve(cwd, sessionID); err != nil {
			return "", err
		} else if resolved != sourceRoot {
			return "", refusal("Source binding is immutable; use a new session for a different worktree.")
		}
		return sourceRoot, nil
	}
	current := state.ReadState(cwd, sessionID)
	if current.BoundSourceRoot != nil && *current.BoundSourceRoot != sourceRoot {
		return "", refusal("Cannot replace the session's pinned source worktree.")
	}
	if current.BoundSourceRoot == nil && !slices.Contains([]state.Phase{state.PhaseIdle, state.PhaseI, state.PhaseP, state.PhaseA}, current.Phase) {
		return "", refusal("Bind the source before B. Preserve the old baseline and re-plan before binding.")
	}
	data, err := encode(Binding{Version: 1, OwnerSessionID: sessionID, NativeCwd: nativeCwd, SourceRoot: sourceRoot, CommonDir: source.commonDir, GitDir: source.gitDir})
	if err != nil {
		return "", err
	}
	path, err := bindingPath(cwd, sessionID)
	if err != nil {
		return "", err
	}
	if _, err = crwdir.EnsureDir(cwd); err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Join(cwd, crwdir.DirName, SourcesSubdir), 0o777); err != nil {
		return "", err
	}
	if _, err = bindingPath(cwd, sessionID); err != nil {
		return "", err
	}
	if err = publish(path, data); err != nil {
		return "", err
	}
	if resolved, err := Resolve(cwd, sessionID); err != nil {
		return "", err
	} else if resolved != sourceRoot {
		return "", refusal("Another source binding won publication; existing binding preserved.")
	}
	return sourceRoot, nil
}

// encode is JSON.stringify(binding, null, 2) and a newline: HTML is not escaped and U+2028 and U+2029 are written as they are,
// where encoding/json escapes them. A backslash and the byte after it are copied together, so an escaped backslash before
// "u2028" stays text.
func encode(b Binding) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		return nil, err
	}
	in, out := buf.Bytes(), make([]byte, 0, buf.Len())
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] != '\\':
			out = append(out, in[i])
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out, i = append(out, "\u2028"...), i+5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out, i = append(out, "\u2029"...), i+5
		default:
			out, i = append(out, in[i], in[i+1]), i+1
		}
	}
	return out, nil
}

// publish writes data as path without ever replacing a file that is there: the whole content goes into an exclusive temp
// file (mode 0600) beside it, which is hard-linked to path (an existing path is left as it is) and removed, so a reader
// sees no file or a complete one. A failed write leaves the temp file behind, as the oracle does.
func publish(path string, data []byte) error {
	tmp := path + "." + rand.Text() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Close(); err != nil {
		return err
	}
	linkErr := os.Link(tmp, path)
	if err = os.Remove(tmp); err != nil {
		return err
	}
	if linkErr != nil && !errors.Is(linkErr, fs.ErrExist) {
		return linkErr
	}
	return nil
}
