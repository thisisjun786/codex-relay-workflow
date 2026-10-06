package spawn

import (
	"context"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// This file ports CXC v0.2.40's entry point for the spawn PreToolUse hook (subagent-config/src/spawn-attach-hook.ts:1117-1150:
// readStdin and main), with the CRW names of contract/schema/cxc/name-substitution.json. The dispatch it calls is
// RunSpawnAttachHook (hook_route.go), whose own 4 MiB check (:845-852) this leg answers before it gets there. Nothing here
// registers, or is, a hook: the row in cmd/crw/component_hooks.go is the entry, and the activation surface stays untouched.
// Differences from the oracle, each recorded in docs/port-cxc/known-defects.md:
//   - the oracle reads 64 KiB chunks and stops as soon as the total passes 4 MiB; harness.ReadStdin reads at most 4 MiB + 1
//     bytes and then reports the overflow. The answer and the bound are the same, and the sibling legs use the same function;
//   - the oracle's process ends at once on SIGINT, where this answers harness.Interrupted (130) and the caller exits with it;
//   - an interrupt after the read but before the answer writes nothing, where the oracle writes nothing only if it was still
//     reading.

const (
	// spawnHookEvent is the event this leg is registered for and records, the oracle's process.argv[3].
	spawnHookEvent = "pre-tool-use"

	// spawnHookOversizedInputReason is the oracle's one refusal text for an input over the bound (:851 and :1143), shared by
	// this leg and RunSpawnAttachHook's own check.
	spawnHookOversizedInputReason = "crw spawn policy input exceeded 4 MiB; refusing to bypass the recursion and trust boundary"
)

// RunHook is the oracle's main: read the hook's input, record the invocation unless the input overflowed, and write
// RunSpawnAttachHook's answer when it has one. It adds no newline of its own (the oracle's `if (out) process.stdout.write(out)`);
// every envelope it writes already ends with one.
func RunHook(ctx context.Context, in io.Reader, out io.Writer, env host.LookupEnv) int {
	raw, overflow := harness.ReadStdin(in)
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	if overflow {
		// The oracle drops the input and records nothing for it, then refuses before parsing.
		_, _ = io.WriteString(out, DenyEnvelope(spawnHookOversizedInputReason))
		return 0
	}
	harness.RecordInvocation(raw, "subagent-config", spawnHookEvent, env)
	if answer := RunSpawnAttachHook(raw, env); answer != "" {
		_, _ = io.WriteString(out, answer)
	}
	return 0
}
