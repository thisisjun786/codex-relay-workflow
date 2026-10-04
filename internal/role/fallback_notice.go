package role

import (
	"context"
	"io"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// DispatchGuidance is verbatim fallback-dispatch-cli.ts:9 after name substitution.
const DispatchGuidance = `Roles with a first fallback use the main-owned dispatch protocol before native spawn. Run crw role helper dispatch with one JSON object on stdin: {action:"start",sessionId:<your native session>,dispatchId:<unique task id>,role:<role>}. Then claim with {action:"claim",sessionId,dispatchId,attemptId}. Only action=spawn authorizes one native call; prepend its marker followed by a newline to the original task/skills, pass candidate model and effort when non-null, and use a fresh context. Report creation with {action:"report",outcome:"created",sessionId,dispatchId,attemptId,agentId:<returned id>}, then use native wait. Report success with {action:"report",outcome:"complete",sessionId,dispatchId,attemptId,agentId} only after validating final work; native completion alone is not task success and terminal reports cannot reopen. Report provider failure with action:"report",outcome:"failed", the same IDs, the original error and executionState (not_created/stopped/unknown/running). For confirmed stagnation or unusable final output, report outcome:"task_failed" with taskFailure:{kind:"stagnation"|"unusable_output",evidence:<concrete task evidence>}, the same IDs, executionState:"stopped", and reconciliation:<termination and partial-work evidence>. Task evidence and reconciliation must each be non-empty text of at most 2000 characters; taskFailure permits only kind and evidence. Omit error on task reports; supplied stop errors still stop, unknown errors reconcile, and next-eligible provider errors must use outcome:"failed". Never relabel cancellation, exhausted bounds, a bare wait timeout or supported disagreement as task failure. Failed handoff requires concrete reconciliation evidence and the recorded agentId for a stopped child. Inspect changes and stop all prior work before retrying. A ready result requires a new claim. Status never authorizes a second spawn. main-direct returns remaining work to the main agent; independent review still requires independent evidence. stop/reconcile never authorizes another model or direct execution. OCX owns provider retries; CRW selects at most two native attempts. Do not invent error codes from arbitrary prose; preserve structured errors or canonical transport error text. Explicit caller model overrides and full-history forks are outside this managed path. Never use a dispatch marker to bypass native permissions.`

// SessionFallbackNotice ports fallback-dispatch-cli.ts:11-21. The existing
// global-only store replaces the oracle's project layer; reads never write it.
func SessionFallbackNotice(env host.LookupEnv) (string, error) {
	return fallbackSessionNotice(env, func() (string, error) { return RenderDispatchCard(env) })
}

func fallbackSessionNotice(env host.LookupEnv, render func() (string, error)) (string, error) {
	config, err := ReadConfig(env)
	if err != nil {
		return "", err
	}
	active := []string{}
	for _, role := range Roles() {
		if config.Roles[role].Fallback != nil {
			active = append(active, string(role))
		}
	}
	parts := []string{}
	if len(active) > 0 {
		parts = append(parts, "[crw] First fallback configured for "+strings.Join(active, ", ")+". "+DispatchGuidance)
	}
	card, err := render()
	if err != nil {
		return "", err
	}
	if card != "" {
		parts = append(parts, card)
	}
	context := strings.Join(parts, "\n")
	if dispatchUTF16Len(context) > 4096 {
		return "", sentinel("SessionStart dispatch context exceeds 4096 characters")
	}
	b, err := Stringify(object{{"hookSpecificOutput", object{{"hookEventName", "SessionStart"}, {"additionalContext", context}}}}, "")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

const fallbackMaxInputBytes = 64 * 1024
const fallbackInterrupted = 130

// RunFallbackNoticeHook owns this component's 64 KiB policy and silent errors.
// The caller supplies existing UTF-8 decoding and metadata observation, keeping
// role independent of harness (which already reaches role through PABCD).
// Only reading is asynchronous: after cancellation it cannot cause late effects.
func RunFallbackNoticeHook(ctx context.Context, in io.Reader, out io.Writer, env host.LookupEnv, ingress func([]byte) string) int {
	input := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(in, fallbackMaxInputBytes+1))
		input <- struct {
			data []byte
			err  error
		}{data, err}
	}()
	var data []byte
	select {
	case result := <-input:
		if result.err != nil || len(result.data) > fallbackMaxInputBytes {
			return 0
		}
		data = result.data
	case <-ctx.Done():
		return fallbackInterrupted
	}
	if ctx.Err() != nil {
		return fallbackInterrupted
	}
	raw := ingress(data)
	payload, err := fallbackJSON(raw)
	if err != nil || payload == nil {
		return 0
	}
	if object, ok := payload.(map[string]any); ok {
		if id, ok := object["agent_id"].(string); ok && id != "" {
			return 0
		}
	}
	answer, err := SessionFallbackNotice(env)
	if ctx.Err() != nil {
		return fallbackInterrupted
	}
	if err == nil {
		_, _ = io.WriteString(out, answer)
	}
	return 0
}
