package bridge

import (
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
)

// The sandbox-root rule (CRW-1265) no longer refuses anything here (CRW-1308): every path that hands a cwd or a writable root to
// the App Server asks the rule and hands its verdict to sandboxroot.Warn, which records it and leaves a warning line; the request
// goes on as it would for an allowed root.

// policyWritableRoots are the writable roots a sandbox policy lists.
func policyWritableRoots(policy map[string]any) []string {
	var out []string
	switch roots := policy["writableRoots"].(type) {
	case []any:
		for _, root := range roots {
			if text, ok := root.(string); ok {
				out = append(out, text)
			}
		}
	case []string:
		out = append(out, roots...)
	}
	return out
}

// writableRoots is every root a request lets the sandbox write: the runtime workspace roots and the
// policy's writable roots.
func writableRoots(roots []string, policy map[string]any) []string {
	return append(append([]string(nil), roots...), policyWritableRoots(policy)...)
}

// absolute keeps the absolute paths: a relative root is the settings contract's refusal to word, with
// its own message, so the sandbox rule is asked about the roots that can be judged.
func absolute(paths []string) []string {
	var out []string
	for _, path := range paths {
		if filepath.IsAbs(path) {
			out = append(out, path)
		}
	}
	return out
}

// effectiveSandbox is the sandbox a request runs under: the mode it states, else the policy's type.
// A request that states neither runs under whatever the thread already has, so it is the caller's
// to resolve (reported, then Unknown).
func effectiveSandbox(sandbox string, policy map[string]any, reported ...any) string {
	if sandbox != "" {
		return sandbox
	}
	if kind := sandboxroot.SandboxOf(policy); policy != nil && kind != sandboxroot.Unknown {
		return kind
	}
	for _, one := range reported {
		if kind := sandboxroot.SandboxOf(one); kind != sandboxroot.Unknown {
			return kind
		}
	}
	return sandboxroot.Unknown
}

// reportedSandbox is the sandbox a thread/read answer reports for its thread, if it does.
func reportedSandbox(state map[string]any) []any {
	return []any{pyjson.Map(state["thread"])["sandbox"], state["sandbox"]}
}

// reportedRoots are the writable roots a thread/read answer reports for its thread.
func reportedRoots(state map[string]any) []string {
	return append(sandboxroot.RootsOf(pyjson.Map(state["thread"])), sandboxroot.RootsOf(state)...)
}

// warnCreated asks the rule about the writable roots the host's answer to thread/start says the new thread got (CRW-1265), and
// warns about a verdict (CRW-1308). A creation that states no policy and no roots leaves them to the host's configuration, which
// may hold the account home; the roots the request names were asked about before the host was called, these are the ones it added.
func warnCreated(created map[string]any, sandbox string) {
	roots := append(sandboxroot.RootsOf(created), sandboxroot.RootsOf(pyjson.Map(created["thread"]))...)
	sandboxroot.Warn("bridge/create_thread:thread/start", sandboxroot.CheckAll("", roots, effectiveSandbox(sandbox, nil, reportedSandbox(created)...)))
}

// warnResumed asks the rule about the cwd and the writable roots the host's answer to thread/resume says the thread now runs with
// (CRW-1265), the way warnCreated asks about thread/start, and warns about a verdict (CRW-1308). A resume that states no policy and
// no roots keeps the host's configured ones, which may hold the account home.
func warnResumed(resumed map[string]any, sandbox string, policy map[string]any) {
	thread := pyjson.Map(resumed["thread"])
	roots := append(sandboxroot.RootsOf(resumed), sandboxroot.RootsOf(thread)...)
	cwd := pyjson.Text(resumed["cwd"])
	if cwd == "" {
		cwd = pyjson.Text(thread["cwd"])
	}
	sandboxroot.Warn("bridge/send_message_to_thread:thread/resume", sandboxroot.CheckAll(cwd, roots, effectiveSandbox(sandbox, policy, reportedSandbox(resumed)...)))
}
