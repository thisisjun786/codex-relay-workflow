package bridge

import (
	"errors"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
)

// rootRefusal turns a sandbox-root refusal (CRW-1265) into the error a receipt records: an RPC-shaped
// failure whose object carries the refusal's code and fields, so a refused request names the path,
// the real path, the rule and the allowed bases. Any other error is returned as it is.
func rootRefusal(method string, err error) error {
	var refusal *sandboxroot.Refusal
	if !errors.As(err, &refusal) {
		return err
	}
	object := map[string]any{"code": refusal.Code(), "message": refusal.Error(), "path": refusal.Path, "resolved": refusal.Resolved, "rule": refusal.Rule, "allowed": anyStrings(refusal.Allowed)}
	if refusal.Project != "" {
		object["project"] = refusal.Project
	}
	return &appserver.RPCError{Method: method, Message: refusal.Error(), Object: object}
}

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

// createdRefusal judges the writable roots the host's answer to thread/start says the new thread got
// (CRW-1265). A creation that states no policy and no roots leaves them to the host's configuration, which may
// hold the account home; the roots the request names were judged before the host was called, these are the ones
// it added. A refusal withholds the title and the initial prompt; the thread stays retained.
func createdRefusal(created map[string]any, sandbox string) error {
	roots := append(sandboxroot.RootsOf(created), sandboxroot.RootsOf(pyjson.Map(created["thread"]))...)
	err := rootRefusal("thread/start", sandboxroot.CheckAll("", roots, effectiveSandbox(sandbox, nil, reportedSandbox(created)...)))
	var rpc *appserver.RPCError
	if errors.As(err, &rpc) {
		rpc.Message += "; initial prompt withheld. Inspect creation receipt. The thread remains retained."
		rpc.Object["message"] = rpc.Message
	}
	return err
}
