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

	// SpawnHookOutputFailed is the status of a leg whose answer could not be written whole (CRW-1122, CRW-1118): the blocking status
	// of a PreToolUse hook, so the host does not run the spawn on an answer it never got. What the event committed before the write (the
	// managed attempt issued to its tool use id, a spent grant and the event's record) stays, so the same call delivered again is
	// answered as the first time and nothing is issued or minted twice; no grant is given back.
	SpawnHookOutputFailed = 2
)

// RunHook is the oracle's main: read the hook's input, record the invocation unless the input overflowed, and write
// RunSpawnAttachHook's answer when it has one. It adds no newline of its own (the oracle's `if (out) process.stdout.write(out)`);
// every envelope it writes already ends with one.
//
// The read runs on its own goroutine so the interrupt wins: the oracle's Node process ends at once on SIGINT, even while it
// waits for its input, so an input that never arrives cannot keep this leg alive. harness.Hook and the sibling component legs
// (job, recall, affordance) answer the same way. A read that completes after the interrupt is neither recorded nor answered.
func RunHook(ctx context.Context, in io.Reader, out io.Writer, env host.LookupEnv) int {
	done := make(chan int, 1)
	go func() { done <- runHook(ctx, in, out, env) }()
	select {
	case code := <-done:
		return code
	case <-ctx.Done():
		return harness.Interrupted
	}
}

func runHook(ctx context.Context, in io.Reader, out io.Writer, env host.LookupEnv) int {
	raw, overflow := harness.ReadStdin(in)
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	if overflow {
		// The oracle drops the input and records nothing for it, then refuses before parsing.
		return spawnHookWrite(out, DenyEnvelope(spawnHookOversizedInputReason))
	}
	harness.RecordInvocation(raw, "subagent-config", spawnHookEvent, env)
	if answer := RunSpawnAttachHook(raw, env); answer != "" {
		return spawnHookWrite(out, answer)
	}
	return 0
}

// spawnHookWrite writes answer whole. A write that fails or stops short is not a success: the answer may carry a deny or the managed
// candidate, and the event has already committed what it needed (SpawnHookOutputFailed).
func spawnHookWrite(out io.Writer, answer string) int {
	if n, err := io.WriteString(out, answer); err != nil || n < len(answer) {
		return SpawnHookOutputFailed
	}
	return 0
}
