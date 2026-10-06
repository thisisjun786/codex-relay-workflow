// AUTOMATION-OWNERSHIP-01 (pabcd-state/src/automation-ownership-gate.ts, CXC v0.2.40, 3c1459ac):
// a narrow PreToolUse safeguard over the automation_update tool, not atomic host-wide
// authorization. target_thread_id is a protected task association, not authenticated creator
// identity; disabled hooks, non-hooked nested tools, UI/file writes and TOCTOU remain outside it.
// The ownership store is only ever read (automation_store.go).
//
// The leg is Guard with Recover false, so a panic would end the hook instead of denying; the
// oracle's try/catch is the deferred recover below, which answers the same generic envelope.
package hook

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// automationMaxPayloadBytes is the gate's own bound (automation-ownership-gate.ts:17), below the
// harness's stdin cap; the harness answers a payload over its own limit before this handler runs.
const automationMaxPayloadBytes = 1024 * 1024

// automationUnverifiable is the catch-all reason of automation-ownership-gate.ts:90-93.
const automationUnverifiable = "Cannot verify automation ownership from the native payload and supported local store. No mutation is permitted."

// automationUpdateToolName is the exact matcher set of automation-ownership-gate.ts:12-16. Never
// search arbitrary tool source or guess additional namespace aliases.
func automationUpdateToolName(name string) bool {
	switch name {
	case "mcp__codex_app__automation_update", "codex_app.automation_update", "codex_app_automation_update",
		"mcp__codex_app.automation_update", "mcp__codex_app_automation_update", "automation_update":
		return true
	}
	return false
}

// automationInputKey is the oracle's INPUT_KEYS (automation-ownership-gate.ts:18-21).
func automationInputKey(key string) bool {
	switch key {
	case "mode", "id", "kind", "name", "prompt", "rrule", "status", "targetThreadId",
		"destination", "notificationPolicy", "executionEnvironment", "model", "projectId", "reasoningEffort":
		return true
	}
	return false
}

// automationTruthy is JavaScript truthiness, which decides whether a hook event that is not
// PreToolUse passes silently (automation-ownership-gate.ts:74-77): every object and array is
// truthy, as are non-zero numbers and non-empty strings.
func automationTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		// JSON.parse holds a magnitude float64 cannot as Infinity, which is truthy.
		number, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return true
		}
		return number != 0 && !math.IsNaN(number)
	}
	return true
}

// automationStringIs is the oracle's === against a string literal: only a string can be equal, so
// a null, a number or an object never matches, and an interface holding an uncomparable value is
// never compared against another interface.
func automationStringIs(value any, want string) bool {
	text, ok := value.(string)
	return ok && text == want
}

// automationCallerSession is caller.session_id once it has passed automationSafeID.
func automationCallerSession(caller map[string]any) string {
	session, _ := caller["session_id"].(string)
	return session
}

// automationDenyEnvelope is the oracle's envelope (automation-ownership-gate.ts:30-35): the
// PreToolUse deny line, built by the shared JSON.stringify-shaped answer so field order and the
// missing HTML escaping match the oracle byte for byte.
func automationDenyEnvelope(reason string) string {
	return editAnswer("deny", "AUTOMATION-OWNERSHIP-01: "+reason, "")
}

// automationEvaluateMutation is evaluateAutomationMutation (automation-ownership-gate.ts:38-63).
// caller fields must come from native hook metadata, never tool_input; ownership is the store
// snapshot the handler read, or nil when the store was not consulted.
func automationEvaluateMutation(request any, caller map[string]any, ownership *automationOwnershipSnapshot) (allow bool, reason string) {
	object, ok := request.(map[string]any)
	if !ok {
		return false, "Automation mutation input must be an object."
	}
	if automationStringIs(object["mode"], "view") {
		return true, "read-only"
	}
	if !automationSafeID(caller["session_id"]) {
		return false, "Native caller session is missing or invalid."
	}
	for _, key := range []string{"agent_id", "agent_type"} {
		if marker, present := caller[key]; present && marker != nil && !automationStringIs(marker, "") {
			return false, "Child or ambiguous caller identity cannot mutate root automations."
		}
	}
	for key := range object {
		if !automationInputKey(key) {
			return false, "Unsupported automation mutation fields."
		}
	}
	if value, present := object["targetThreadId"]; present && !automationStringIs(value, automationCallerSession(caller)) {
		return false, "Retargeting another task is denied."
	}
	if value, present := object["kind"]; present && !automationStringIs(value, "heartbeat") {
		return false, "Only task-associated heartbeats have supported ownership."
	}
	if value, present := object["destination"]; present && !automationStringIs(value, "thread") && !automationStringIs(value, "local") {
		return false, "Unknown automation destination."
	}
	mode, _ := object["mode"].(string)
	if mode == "create" || mode == "suggested_create" {
		if _, present := object["id"]; present || !automationStringIs(object["kind"], "heartbeat") {
			return false, "Create requires a heartbeat without an existing id."
		}
		return true, "heartbeat targets the native caller"
	}
	if mode != "update" && mode != "suggested_update" && mode != "delete" {
		return false, "Unknown automation mutation mode."
	}
	if !automationSafeID(object["id"]) {
		return false, "A safe existing automation id is required."
	}
	id, _ := object["id"].(string)
	if ownership == nil || ownership.ID != id || ownership.Kind != "heartbeat" || ownership.TargetThreadID != automationCallerSession(caller) {
		return false, "Automation ownership is missing, unsupported, conflicting, or belongs to another task."
	}
	return true, "stored heartbeat targets the native caller"
}

// automationCodexHome is env.CODEX_HOME ?? join(homedir(), ".codex")
// (automation-ownership-gate.ts:85-86). A set-but-empty CODEX_HOME is kept, as ?? keeps it, so it
// reaches the store reader's absolute-path refusal rather than falling back to ~/.codex.
func automationCodexHome(env host.LookupEnv) (string, error) {
	if value, set := env("CODEX_HOME"); set {
		return value, nil
	}
	home, err := host.Home(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// HandleAutomationOwnershipGate is handleAutomationOwnershipGate, the dedicated matcher handler:
// the native deny JSON for a mutation this session may not make, else empty. A malformed payload
// fails closed; a known unrelated call passes in silence. The store is read only when the request
// carries a safe id, and the read is injected so a test can fail it.
func HandleAutomationOwnershipGate(raw string, env host.LookupEnv) string {
	return automationGateHandle(raw, env, automationReadOwnership)
}

func automationGateHandle(raw string, env host.LookupEnv, read func(string, string) (*automationOwnershipSnapshot, error)) (out string) {
	// The oracle's catch: no raw tool input, prompt or private path reaches the answer.
	defer func() {
		if recover() != nil {
			out = automationDenyEnvelope(automationUnverifiable)
		}
	}()
	if len(raw) > automationMaxPayloadBytes {
		return automationDenyEnvelope("Hook payload exceeds the supported bound.")
	}
	// JSON.parse: invalid JSON throws and is answered by the catch, while valid JSON that is not an
	// object is the malformed-payload deny below. editObject is this package's reader for exactly
	// this payload shape, and it keeps a number JSON.parse would hold as Infinity (UseNumber) from
	// failing the whole document.
	if !json.Valid([]byte(raw)) {
		return automationDenyEnvelope(automationUnverifiable)
	}
	payload := editObject(raw)
	if payload == nil {
		return automationDenyEnvelope("Malformed native hook payload.")
	}
	if name, isString := payload["tool_name"].(string); isString && !automationUpdateToolName(name) {
		return ""
	}
	if !automationStringIs(payload["hook_event_name"], "PreToolUse") {
		if automationTruthy(payload["hook_event_name"]) {
			return ""
		}
		return automationDenyEnvelope("Native PreToolUse event is missing.")
	}
	if _, isString := payload["tool_name"].(string); !isString {
		return automationDenyEnvelope("Native tool name is missing.")
	}
	request := payload["tool_input"]
	if object, isObject := request.(map[string]any); isObject && automationStringIs(object["mode"], "view") {
		return ""
	}
	// Views, creates and calls without a safe id need no ownership store lookup; an initial deny
	// with a safe id is not answered yet, because the store may still show the caller's own
	// heartbeat, as automation-ownership-gate.ts:80-88 does.
	initialAllow, initialReason := automationEvaluateMutation(request, payload, nil)
	object, isObject := request.(map[string]any)
	if !isObject || !automationSafeID(object["id"]) || initialAllow {
		if initialAllow {
			return ""
		}
		return automationDenyEnvelope(initialReason)
	}
	id, _ := object["id"].(string)
	home, err := automationCodexHome(env)
	if err != nil {
		return automationDenyEnvelope(automationUnverifiable)
	}
	ownership, err := read(home, id)
	if err != nil {
		return automationDenyEnvelope(automationUnverifiable)
	}
	allow, reason := automationEvaluateMutation(request, payload, ownership)
	if allow {
		return ""
	}
	return automationDenyEnvelope(reason)
}
