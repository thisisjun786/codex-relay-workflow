//go:build dev

package ci

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CRW-964: the pre-push hook. The repository is public and CI scans a pushed commit after the
// fact, which cannot stop a leak that has already left the host, so this tool installs a hook that
// refuses a push whose range carries a secret or a blob over the limit. It is installed only where
// the operator asks; the tests install it into temporary repositories alone.

const (
	// localHookName is the hook this tool owns.
	localHookName = "pre-push"
	// localHookMarker is the line the hook carries, so a reader can tell this tool's hook from a
	// foreign one. The whole script is compared for the current version; the marker under the
	// shebang is what recognises an older script this tool wrote.
	localHookMarker = "# crw-dev ci local pre-push hook (CRW-964)"
	// localGitleaksPin is the Gitleaks release the hook requires: the one scripts/ci/secrets.sh
	// pins and the hosted secrets job runs. A different scanner has different rules, so the hook
	// refuses it rather than approving a range the hosted job would reject after the push.
	localGitleaksPin = "8.30.1"
)

// localHookScript is the hook, written verbatim. It is a shell script so it can run before git
// starts the push, and it fails closed: a missing scanner, an unreadable range or a finding each
// block the push rather than letting it through.
var localHookScript = strings.ReplaceAll(hookScriptTemplate, "@CRW964_GITLEAKS_PIN@", localGitleaksPin)

// hookScriptTemplate is the hook before the pinned Gitleaks version is substituted.
const hookScriptTemplate = `#!/usr/bin/env bash
# crw-dev ci local pre-push hook (CRW-964)
# Refuses a push whose range brings a secret or a blob over 2 MiB into the history. The range is
# <remote sha>..<local sha>; when the remote sha is the all-zeros object the whole local sha is
# judged. A missing scanner, an unreadable range and a finding each exit non-zero.
set -euo pipefail

limit=$((2 * 1024 * 1024))
root=$(git rev-parse --show-toplevel)
git_dir=$(git rev-parse --absolute-git-dir)
scanner=${CRW_CI_GITLEAKS:-gitleaks}
expected=${CRW_CI_GITLEAKS_VERSION:-@CRW964_GITLEAKS_PIN@}

status=0
while read -r local_ref local_sha remote_ref remote_sha; do
  [ -n "${local_sha:-}" ] || continue
  case "$local_sha" in
    *[!0]*) : ;;
    *) continue ;;  # an all-zero local sha (SHA-1 or SHA-256): the ref is being deleted
  esac
  if [ -z "${remote_sha:-}" ] || [ -z "${remote_sha//0/}" ]; then
    range="$local_sha"
  else
    range="$remote_sha..$local_sha"
  fi

  # The large-blob check: every blob the range brings in, over the limit, blocks the push. The
  # range is read first and its exit status checked, so a range git cannot read is refused rather
  # than read as no offenders.
  if ! listing=$(git -C "$root" rev-list --objects --no-object-names "$range"); then
    echo "pre-push: could not read the range $range" >&2
    exit 1
  fi
  offenders=$(printf '%s\n' "$listing" |
    git -C "$root" cat-file --batch-check='%(objecttype) %(objectname) %(objectsize)' |
    awk -v limit="$limit" '$1 == "blob" && $3 > limit { print $2, $3 }') || {
      echo "pre-push: could not size the range $range" >&2
      exit 1
    }
  if [ -n "$offenders" ]; then
    echo "pre-push: a blob over $limit bytes is in $range:" >&2
    printf '%s\n' "$offenders" | while read -r oid size; do
      path=$(git -C "$root" rev-list --objects "$range" | awk -v oid="$oid" '$1 == oid { print $2; exit }')
      echo "  $oid ($size bytes) $path" >&2
    done
    echo 'pre-push: shrink the file or rebuild the branch without it; a public history cannot drop it' >&2
    status=1
  fi

  # The secret scan: the same Gitleaks the hosted secrets job pins, over the same range. The
  # pinned release is required: an older scanner has different rules and could approve a range the
  # hosted job rejects after the push has already left the host.
  if ! command -v "$scanner" >/dev/null 2>&1; then
    echo "pre-push: $scanner is not on PATH (set CRW_CI_GITLEAKS); refusing the push rather than scanning nothing" >&2
    exit 1
  fi
  found=$("$scanner" version 2>/dev/null || true)
  if [ "$found" != "$expected" ]; then
    echo "pre-push: $scanner is version '$found', but this repository pins $expected; refusing the push" >&2
    exit 1
  fi
  ignore=$(mktemp)
  trap 'rm -f "$ignore"' EXIT
  if ! "$scanner" git "$git_dir" --config "$root/.gitleaks.toml" \
    --gitleaks-ignore-path "$ignore" --ignore-gitleaks-allow \
    --log-opts="-m $range" --redact --no-banner; then
    echo "pre-push: Gitleaks found a secret in $range" >&2
    status=1
  fi
done

if [ "$status" -ne 0 ]; then
  echo 'pre-push: the push is blocked' >&2
fi
exit "$status"
`

// localHook parses `crw-dev ci local hook <install|status> [--root <dir>]`.
func localHook(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "local hook: error: the following arguments are required: install, status")
		return 2
	}
	action := args[0]
	flags := newFlags("local hook " + action)
	root := flags.String("root", ".", "the repository whose hook to install")
	if code := parseFlags(flags, "Install or inspect the crw-dev ci local pre-push hook.", args[1:], stdout, stderr); code >= 0 {
		return code
	}
	path, err := localHookPath(*root)
	if err != nil {
		return failf(stderr, "local hook: %s", err)
	}
	switch action {
	case "install":
		state, err := localHookInstall(path)
		if err != nil {
			return failf(stderr, "local hook: %s", err)
		}
		fmt.Fprintf(stdout, "local hook: %s at %s\n", state, path)
		return 0
	case "status":
		fmt.Fprintf(stdout, "local hook: %s at %s\n", localHookState(path), path)
		return 0
	default:
		fmt.Fprintf(stderr, "local hook: error: invalid choice: %q (choose from install, status)\n", action)
		return 2
	}
}

// localHookPath is the hook's path in the repository the directory belongs to. It asks git rather
// than assuming .git/hooks, so a linked worktree and a custom hooks directory are handled.
func localHookPath(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--git-path", "hooks/"+localHookName)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return path, nil
}

// localHookState is what the hook's path holds: installed (this tool's hook, the current script or
// an older one it wrote), absent, or foreign (a hook this tool did not write, which it leaves
// alone).
func localHookState(path string) string {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "absent"
	}
	if err != nil {
		return "unreadable"
	}
	if bytes.Equal(data, []byte(localHookScript)) {
		return "installed"
	}
	if localHookOwned(data) {
		return "installed"
	}
	return "foreign"
}

// localHookOwned reports whether data is a hook this tool wrote: a shell script whose second line
// is the marker. The marker alone is not trusted (a foreign hook could carry the line by accident),
// so the file must also open like the script this tool writes.
func localHookOwned(data []byte) bool {
	first, rest, _ := bytes.Cut(data, []byte("\n"))
	if !bytes.Equal(bytes.TrimRight(first, "\r"), []byte("#!/usr/bin/env bash")) {
		return false
	}
	second, _, _ := bytes.Cut(rest, []byte("\n"))
	return bytes.Equal(bytes.TrimRight(second, "\r"), []byte(localHookMarker))
}

// localHookInstall writes the hook, refusing to replace a hook this tool did not write. It is
// idempotent: this tool's own hook is rewritten, so an updated script reaches a repository that
// already carries an older one.
func localHookInstall(path string) (string, error) {
	switch localHookState(path) {
	case "installed", "absent":
		// Rewrite or install.
	default:
		return "", fmt.Errorf("%s already exists and was not written by this tool; leaving it untouched", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// Write to a sibling and rename, so a failure cannot leave a half-written hook that git
	// would then refuse to run.
	temp := path + ".tmp"
	if err := os.WriteFile(temp, []byte(localHookScript), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(temp, path); err != nil {
		return "", err
	}
	return "installed", nil
}

// localHookRun runs the installed hook over a range, as git would. It is what the tests use to
// prove a push carrying a secret is blocked; the hook's own script does the work.
func localHookRun(root, localRef, localSHA, remoteRef, remoteSHA string, env []string) (int, string) {
	path, err := localHookPath(root)
	if err != nil {
		return 1, err.Error()
	}
	cmd := exec.Command("bash", path)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("%s %s %s %s\n", localRef, localSHA, remoteRef, remoteSHA))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out.String()
	}
	if err != nil {
		return 1, err.Error()
	}
	return 0, out.String()
}
