// Package gitprobe is the policy crw runs Git under when it reads a repository it does not own: the
// environment, the global options and the time bound the bridge's managed worktrees introduced, shared so the
// session source binding, the source identity capture and the bridge verify one worktree the same way.
//
// Two environments are offered. ProbeEnv is the read-only probe's: every inherited GIT_* variable removed, the
// probe made non-interactive, then the caller's trusted overrides. Sanitize is the common base for a caller that
// runs more than probes (premerge fetches over the network, so it keeps the transport variables): only the
// variables that point git at another repository, object store, index, namespace or discovery boundary, or that
// inject configuration, are removed, then the overrides. Both apply an override once, after the safe default, so
// an override is never stripped and never doubled.
package gitprobe

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Timeout bounds one probe, as the bridge bounds its worktree commands.
const Timeout = 30 * time.Second

// routing is every variable that makes git read another repository than the one a probe names, or that
// changes what it finds there: the repository, work tree, common and object directories, index, namespace,
// discovery boundary and replacement objects, and configuration injected through the environment.
var routing = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_INDEX_VERSION",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_QUARANTINE_PATH",
	"GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_PREFIX", "GIT_IMPLICIT_WORK_TREE", "GIT_INTERNAL_SUPER_PREFIX",
	"GIT_SHALLOW_FILE", "GIT_GRAFT_FILE", "GIT_REPLACE_REF_BASE", "GIT_NO_REPLACE_OBJECTS",
	"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
}

// IsRouting says whether name is a variable Sanitize removes: one of the routing variables, or a numbered
// GIT_CONFIG_KEY_<n> or GIT_CONFIG_VALUE_<n>.
func IsRouting(name string) bool {
	return slices.Contains(routing, name) || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// GlobalOptions are the options a probe passes before its command: no optional locks (a probe never refreshes
// the index), no replacement objects, hooks pointed at the null device, no submodule recursion and no file
// system monitor, so a probe runs no program of the repository's.
func GlobalOptions() []string {
	return []string{"--no-optional-locks", "--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull, "-c", "submodule.recurse=false", "-c", "core.fsmonitor=false"}
}

// Args is a probe's argument list for git run in cwd: GlobalOptions, -C cwd, then args.
func Args(cwd string, args ...string) []string {
	return append(append(GlobalOptions(), "-C", cwd), args...)
}

// ProbeEnv is the read-only probe's environment: base (nil is the process environment) without any GIT_*
// variable, after GIT_TERMINAL_PROMPT=0 and GIT_NO_LAZY_FETCH=1, then overrides.
func ProbeEnv(base []string, overrides ...string) []string {
	if base == nil {
		base = os.Environ()
	}
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1"}
	for _, entry := range base {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return override(env, overrides)
}

// Sanitize is the common base: base (nil is the process environment) without the routing variables, then
// overrides. Identity, transport and configuration-file variables stay.
func Sanitize(base []string, overrides ...string) []string {
	if base == nil {
		base = os.Environ()
	}
	return override(slices.DeleteFunc(slices.Clone(base), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return IsRouting(name)
	}), overrides)
}

// override drops every entry an override names and appends the overrides in order, so each is applied once.
func override(env, overrides []string) []string {
	if len(overrides) == 0 {
		return env
	}
	names := make([]string, 0, len(overrides))
	for _, entry := range overrides {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	env = slices.DeleteFunc(env, func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains(names, name)
	})
	return append(env, overrides...)
}

// Command is git run in cwd under the probe policy, bound by ctx: Args, ProbeEnv with overrides, and a second
// for its output pipes to close once the process has ended. The caller bounds ctx (Timeout is the bridge's).
func Command(ctx context.Context, cwd string, overrides []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", Args(cwd, args...)...)
	cmd.Env = ProbeEnv(nil, overrides...)
	cmd.WaitDelay = time.Second
	return cmd
}
