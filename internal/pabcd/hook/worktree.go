package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// The Codex app creates one worktree per thread under <CODEX_HOME>/worktrees/<slot>/<repo>. This file is the Go
// form of CXC v0.2.40 pabcd-state/src/worktree-guard.ts:1-185 and 503-567: finding out whether a cwd is inside
// one, and the two context hooks that rest on it (WORKTREE-GUARD-01 at SessionStart, WORKTREE-GUARD-02 at
// UserPromptSubmit). The deletion guard of that file is a separate port that reuses WorktreeIdentity,
// detectManagedWorktree and parseRaw.

// worktreeGuardDir is the directory under the state directory that holds the once-per-session markers.
const worktreeGuardDir = "worktree-guard"

// WorktreeIdentity says where a cwd sits among the managed worktrees. CheckoutRoot is empty, the oracle's null,
// when no .git entry was found between the cwd and the slot root.
type WorktreeIdentity struct {
	Managed      bool
	WorktreesDir string
	Slot         string
	SlotRoot     string
	CheckoutRoot string

	cwd string // the real path of the payload's cwd when that exists, else empty: the marker's workspace is opened on it, not on the payload's cwd
}

// resolveCodexHome is the trimmed CODEX_HOME, else ~/.codex; the error is a home directory Node could not read.
func resolveCodexHome(env host.LookupEnv) (string, error) {
	if value, _ := env("CODEX_HOME"); text.Trim(value) != "" {
		return text.Trim(value), nil
	}
	home, err := host.Home(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// candidateWorktreeRoots is the default root followed by the entries of CRW_WORKTREE_ROOTS (CODEXCLAW_WORKTREE_ROOTS
// in CXC), split on the platform's list separator, trimmed and without the empty ones.
func candidateWorktreeRoots(env host.LookupEnv) ([]string, error) {
	codexHome, err := resolveCodexHome(env)
	if err != nil {
		return nil, err
	}
	roots := []string{filepath.Join(codexHome, "worktrees")}
	extra, _ := env("CRW_WORKTREE_ROOTS")
	for _, entry := range strings.Split(extra, string(os.PathListSeparator)) {
		if entry = text.Trim(entry); entry != "" {
			roots = append(roots, entry)
		}
	}
	return roots, nil
}

// resolvePath is the real path of p, and whether p itself exists. When it does not, the path is that of its nearest
// existing ancestor with the missing rest appended, and the walk gives up with the lexical path after 65 missing
// levels. Oracle defect, kept: the rest is cut with abs.slice(cur.length + 1), which also drops the first character of
// the first missing segment when the ancestor is the filesystem root ("/nonexistent" becomes "/onexistent"); cur is a
// prefix of abs, so the cut is the prefix and one character. Such a path can name another real directory, which is why
// only an exact path is ever written under.
func resolvePath(p string) (path string, exact bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p, false
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, true
	}
	cur := abs
	for missing := 1; ; missing++ {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, false
		}
		cur = parent
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			rest := abs[len(cur):]
			_, skip := utf8.DecodeRuneInString(rest)
			if rest = rest[skip:]; rest != "" {
				return filepath.Join(real, rest), false
			}
			return real, false
		}
		if missing > 64 {
			return abs, false
		}
	}
}

func canonicalize(p string) string {
	path, _ := resolvePath(p)
	return path
}

func firstSegment(rel string) string {
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return rel
}

// findCheckoutRoot is the nearest ancestor of cwd (itself included) that holds a .git entry, looking no higher than
// slotRoot; empty when there is none.
func findCheckoutRoot(cwd, slotRoot string) string {
	for cur := cwd; ; {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		if cur == slotRoot {
			return ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// detectManagedWorktree places cwd under the first candidate root that holds it, by canonical paths, so a link into a
// slot counts and a link out of it does not. A home directory that cannot be read leaves nothing managed.
func detectManagedWorktree(cwd string, env host.LookupEnv) WorktreeIdentity {
	if text.Trim(cwd) == "" {
		return WorktreeIdentity{}
	}
	roots, err := candidateWorktreeRoots(env)
	if err != nil {
		return WorktreeIdentity{}
	}
	canonicalCwd, exact := resolvePath(cwd)
	for _, root := range roots {
		canonicalRoot := canonicalize(root)
		if canonicalCwd == canonicalRoot || !strings.HasPrefix(canonicalCwd, canonicalRoot+"/") {
			continue
		}
		slot := firstSegment(canonicalCwd[len(canonicalRoot)+1:])
		if slot == "" {
			continue
		}
		slotRoot := filepath.Join(canonicalRoot, slot)
		id := WorktreeIdentity{Managed: true, WorktreesDir: canonicalRoot, Slot: slot, SlotRoot: slotRoot, CheckoutRoot: findCheckoutRoot(canonicalCwd, slotRoot)}
		if exact {
			id.cwd = canonicalCwd
		}
		return id
	}
	return WorktreeIdentity{}
}

// detectRenameIntent is RENAME_INTENT: the prompt names a worktree and says rename. The oracle's /i has no /u, so
// only ASCII letters fold (the Kelvin sign is not a k), which foldASCII keeps.
func detectRenameIntent(prompt string) bool {
	folded := foldASCII(prompt)
	has := func(words ...string) bool {
		for _, word := range words {
			if strings.Contains(folded, word) {
				return true
			}
		}
		return false
	}
	return has("worktree", "워크트리") && has("rename", "re-name", "이름", "명명", "바꾸", "바꿔", "지어", "짓")
}

func buildSessionStartContext(id WorktreeIdentity, cwd string) string {
	if !id.Managed {
		return ""
	}
	checkout := "unconfirmed (no .git entry found)"
	gitAdvice := "- Stay here and commit early. The app thread title is renamed by the user in\n  the app sidebar — agents cannot rename it."
	if id.CheckoutRoot != "" {
		checkout = id.CheckoutRoot
		gitAdvice = "- To name things, ADOPT IN PLACE: stay here; `git switch -c <name>` (detached) or\n  `git branch -m <name>` names the branch; commit early. The app thread title is\n  renamed by the user in the app sidebar — agents cannot rename it."
	}
	return strings.Join([]string{
		"[crw: MANAGED WORKTREE — identity guard (WORKTREE-GUARD-01)]",
		"This session runs inside a Codex-app-managed worktree: " + checkout,
		"(cwd: " + cwd + "; slot: " + id.SlotRoot + "; worktrees root: " + id.WorktreesDir + ").",
		"- This thread is BOUND to this worktree. NEVER delete, recreate, or \"start fresh\"",
		"  to rename it — that destroys uncommitted work and breaks the app binding.",
		"- App worktrees usually start detached-HEAD: the \"worktree name\" is the directory",
		"  slot, not a branch. branch ≠ worktree ≠ thread title (three namespaces).",
		gitAdvice,
		"- Do NOT `git worktree move` the ACTIVE worktree: it invalidates this session's",
		"  cwd and app rebinding is not guaranteed. Move only OTHER/inactive worktrees.",
		"- The app may auto-delete this worktree on chat archive (snapshot kept) and",
		"  retains only the latest N managed worktrees: commit early, push on approval.",
		"- Detection covers the default root + CRW_WORKTREE_ROOTS; a custom app",
		"  worktree root needs that env. Full procedures: $crw:crw-worktree-guardian.",
	}, "\n")
}

func buildRenameGuidance(id WorktreeIdentity) string {
	slotRoot := id.SlotRoot
	if slotRoot == "" {
		slotRoot = "unknown"
	}
	return strings.Join([]string{
		"[crw: MANAGED WORKTREE — rename/adopt guidance (WORKTREE-GUARD-02)]",
		"Rename request on a managed worktree. ADOPT IN PLACE — do not delete/recreate:",
		"1. Stay in this worktree. It is bound to the app thread; recreating breaks that.",
		"2. Name the BRANCH: `git switch -c <name>` (detached HEAD) or `git branch -m <name>`.",
		"3. Commit the work early — the app auto-deletes managed worktrees on archive",
		"   (snapshot kept) and retains only the latest N.",
		"4. The app THREAD TITLE is renamed by the user in the app sidebar; neither the",
		"   directory nor the branch rename changes it.",
		"5. Do NOT `git worktree move` the ACTIVE worktree (session cwd dies). Move/repair",
		"   are for OTHER inactive worktrees: `git worktree move <old> <new>`,",
		"   `git worktree repair <new>` — feature-detect with `-h`, no version gates.",
		"Slot protected by the PreToolUse guard: " + slotRoot + ".",
	}, "\n")
}

// parseRaw is JSON.parse of the hook's input and nothing else: one object, untrimmed (a BOM fails), else nil.
func parseRaw(raw string) map[string]any { return editObject(raw) }

// ensureStateDir is crwdir.EnsureDir (ensureDir in crwdir/crwdir.go) through root, which that package, outside this port's files, cannot be given: the project's state directory is made only when it is missing, an
// existing directory, link or file is left as it is, and only the process that made it publishes its .gitignore. Going
// through root, and not through a path, is what keeps a link planted in the workspace from carrying either write out of it.
func ensureStateDir(root *os.Root) error {
	if err := root.Mkdir(StateDir, 0o777); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	f, err := root.OpenFile(StateDir+"/.gitignore", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err == nil {
		_, err = f.WriteString(crwdir.GitignoreText)
		err = errors.Join(err, f.Close())
	}
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	// The new directory stays when its .gitignore cannot be written, and then no later EnsureDir publishes it: the files kept
	// there show as untracked. CXC removes the directory with rmdir, so its next run retries; a Root has no directory-only
	// removal, and one that removes files could delete what another writer put there meanwhile.
	return err
}

// markerPath is the session's marker, <state dir>/worktree-guard/<key>.json, relative to the workspace root and resolved
// through the links that stay inside it; a link that leaves the workspace is an error.
func markerPath(base, sessionID string) (string, error) {
	dir, err := confined(base, filepath.Join(base, StateDir, worktreeGuardDir))
	if err != nil {
		return "", err
	}
	return confined(base, filepath.Join(base, dir, state.SanitizeKey(sessionID)+".json"))
}

// alreadyInjected is existsSync of the marker: a link that stays inside the workspace is followed, one that leaves it
// or leads nowhere counts as no marker.
func alreadyInjected(root *os.Root, base, sessionID string) bool {
	marker, err := markerPath(base, sessionID)
	if err != nil {
		return false
	}
	_, err = root.Stat(marker)
	return err == nil
}

// markInjected writes the marker if there is none, and fails quietly: a marker that cannot be written never blocks the
// guidance. Intentional change (port: fixed): the oracle writes with writeFileSync, which follows a link and truncates what
// it finds, so a planted link makes it create a file outside the workspace or empty a record; here every step runs through
// root and the marker is created exclusively.
func markInjected(root *os.Root, base, sessionID, slot string) {
	if ensureStateDir(root) != nil {
		return
	}
	dir, err := confined(base, filepath.Join(base, StateDir, worktreeGuardDir))
	if err != nil || root.MkdirAll(dir, 0o777) != nil {
		return
	}
	marker, err := markerPath(base, sessionID)
	if err != nil {
		return
	}
	body, err := role.Stringify(struct {
		InjectedAt string `json:"injectedAt"`
		Slot       string `json:"slot"`
	}{time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), slot}, "")
	if err != nil {
		return
	}
	f, err := root.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(body)
	_ = f.Close()
}

// HandleWorktreeGuard is handleWorktreeGuard, for the SessionStart and UserPromptSubmit legs: the event and the context
// to hand the model, both empty when the hook has nothing to say. The caller wraps them as the hook's answer.
func HandleWorktreeGuard(raw string, env host.LookupEnv) (event, context string) {
	payload := parseRaw(raw)
	cwd, _ := payload["cwd"].(string)
	if cwd == "" {
		return "", ""
	}
	switch payload["hook_event_name"] {
	case "SessionStart":
		return "SessionStart", buildSessionStartContext(detectManagedWorktree(cwd, env), cwd)
	case "UserPromptSubmit":
		prompt, _ := payload["prompt"].(string)
		sessionID, _ := payload["session_id"].(string)
		id := detectManagedWorktree(cwd, env)
		if !id.Managed || !detectRenameIntent(prompt) {
			return "", ""
		}
		if sessionID != "" {
			// The workspace is the canonical cwd detection found, opened once: a link retargeted after detection
			// decides nothing, and every write below goes through this root.
			if root, err := os.OpenRoot(id.cwd); id.cwd != "" && err == nil {
				defer root.Close()
				if alreadyInjected(root, id.cwd, sessionID) {
					return "", ""
				}
				markInjected(root, id.cwd, sessionID, id.Slot)
			}
		}
		return "UserPromptSubmit", buildRenameGuidance(id)
	}
	return "", ""
}
