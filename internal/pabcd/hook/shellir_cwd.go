package hook

import (
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// shellirPayloadCwd is the directory the command readers judge a command in: the payload's cwd when it is an absolute path
// to an existing directory, and "" otherwise. An empty, relative or missing cwd makes the directory unknown, so a relative
// target is never resolved against the hook's own process directory or taken as the literal path (CRW-1028 item 7).
func shellirPayloadCwd(cwd string) string {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	st, err := os.Stat(cwd)
	if err != nil || !st.IsDir() {
		return ""
	}
	return cwd
}

// ShellCommandReadable reports whether the shared command reader can read a shell command run in the payload's cwd, the reading
// every command gate starts from. A command it cannot read is refused by each gate. The differential fuzz uses it to count the
// unreadable cases of a run and the ones the Go side refused (criterion c2g).
func ShellCommandReadable(command, cwd string, env host.LookupEnv) bool {
	_, err := shellir.AnalyzeEnv(command, shellirPayloadCwd(cwd), env)
	return err == nil
}

// WorktreeCwdManaged reports whether the worktree deletion guard treats a cwd as the checkout of a managed worktree, the only place
// it judges a command. The differential fuzz uses it to tell a command the guard declined to read from one it refused.
func WorktreeCwdManaged(cwd string, env host.LookupEnv) bool {
	return cwd != "" && detectManagedWorktree(cwd, env).Managed
}
