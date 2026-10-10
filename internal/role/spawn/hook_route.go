package spawn

import (
	"cmp"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// This file ports the second half of CXC v0.2.40's runSpawnAttachHook (subagent-config/src/spawn-attach-hook.ts:849-852 and :991-1115:
// the 4 MiB input bound, role routing, the ciphertext restore, the item re-assembly, the notices and the output envelope) over the
// assembly of hook.go, with the CRW names of contract/schema/cxc/name-substitution.json. The managed dispatch loop itself lives in
// hook_managed.go; here are the final-gate call (:1072-1079), the managed guards on the notice and the no-op, and the issuance with
// the candidate model and effort (:1094-1102). Nothing here registers, or is, a hook. Differences from the oracle, each recorded in
// docs/port-cxc/known-defects.md:
//   - the project layer is dropped (decision 7), so the trust warning is empty: trustPrefix is applied, but nothing sets it;
//   - the oracle's :1030-1046 branches for an empty guard are ported in spawnHookRoutePrompt, but the guard is never empty;
//   - a managed spawn whose tool_input nests too deep to be answered is denied before it is issued (CRW-1122);
//   - a tool_input nested past 4,463 levels prints nothing, where the oracle's JSON.stringify fails past 4,462 levels of junk inside
//     tool_input at Node 24's default stack, measured by the spawn recorder (the threshold depends on the stack size, the platform
//     and the depth of the caller, CRW-749); the payload is still read at any depth, as JSON.parse reads it, so a subagent's
//     recursion is still denied (spawnHookRouteLoad keeps the parser from recursing over a depth it cannot hold).

const (
	spawnHookRouteMaxInput = 4 * 1024 * 1024
	spawnHookRouteMaxDepth = 4463 // V8's JSON.stringify fails past 4,462 levels of junk inside tool_input at Node 24's default stack (tool_input is level 1)
)

// RunSpawnAttachHook is runSpawnAttachHook: the hook output for one PreToolUse payload, "" to allow it untouched, a deny envelope, or
// an allow envelope whose updatedInput replaces tool_input. It is total: a payload that does not parse, or a panic, prints nothing.
// raw is the stdin text already decoded as Node decodes it, so len(raw) is Buffer.byteLength(raw).
func RunSpawnAttachHook(raw string, env host.LookupEnv) (out string) {
	if len(raw) > spawnHookRouteMaxInput {
		return DenyEnvelope(spawnHookOversizedInputReason)
	}
	commit := &spawnHookCommit{}
	defer func() {
		if commit.unlock != nil {
			commit.unlock() // the event's lock is held until its answer is recorded
		}
	}()
	defer func() {
		if recover() != nil {
			switch out = ""; {
			case commit.issued:
				out = DenyEnvelope(spawnHookReconcileReason)
			case commit.subagent:
				out = DenyEnvelope(RecurseDenyReason) // a subagent is never let through by a failure, spent grant or not
			}
		}
	}()
	payload, ok := spawnHookRouteLoad(raw)
	if !ok {
		return ""
	}
	a, deny, stop := spawnHookAssembleWith(spawnHookView(payload), env, commit)
	if stop {
		return deny
	}
	return spawnHookRoute(a, env)
}

// spawnHookRouteLoad is JSON.parse((raw ?? "").trim() || "{}") and isRecord: an ordered object, or false. A lone surrogate escape is
// kept, as is a number's spelling (the respelling happens when the answer is written). JSON.parse is iterative and reads any depth,
// but pyjson's parser recurses once per level and a 4 MiB payload can nest 2 million levels, which would end the process in a stack
// overflow that recover cannot catch: a document nested past pyjson.MaxDepth is first cut down by spawnHookRouteShallow.
func spawnHookRouteLoad(raw string) (pyjson.Object, bool) {
	doc := text.Trim(raw)
	if doc == "" {
		doc = "{}"
	}
	if spawnHookRouteNesting(doc) > pyjson.MaxDepth {
		var ok bool
		if doc, ok = spawnHookRouteShallow(doc); !ok {
			return nil, false
		}
	}
	v, err := pyjson.Loads(doc, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers})
	payload, ok := v.(pyjson.Object)
	return payload, err == nil && ok
}

// spawnHookRouteNesting is how deep the brackets of doc nest outside its strings. It counts the text, so it cannot fail or recurse.
func spawnHookRouteNesting(doc string) int {
	depth, deepest, inString := 0, 0, false
	for i := 0; i < len(doc); i++ {
		switch c := doc[i]; {
		case inString:
			if c == '\\' {
				i++
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			depth++
			deepest = max(deepest, depth)
		case c == ']' || c == '}':
			depth--
		}
	}
	return deepest
}

// spawnHookRouteShallow checks doc against the JSON grammar as JSON.parse does at any depth, with an explicit stack (encoding/json's
// token reader refuses past 10,000 levels as well), and writes it back with every container deeper than spawnHookRouteMaxDepth+2
// levels replaced by null. Only the part of tool_input beyond that depth is lost, and spawnHookRouteDeep still sees the levels that
// stay, so the answer is the one JSON.stringify's RangeError gives; a deep field elsewhere is never read.
func spawnHookRouteShallow(doc string) (string, bool) {
	const (
		value       = iota // a value
		arrayFirst         // after [: a value or ]
		objectFirst        // after {: a key or }
		key                // after a comma in an object: a key
		colon              // after a key
		next               // after a value: a comma or the close
	)
	var stack []byte
	var out strings.Builder
	state, copied, cutAt, cutDepth := value, 0, 0, 0
	for i := 0; i < len(doc); {
		c, ok := doc[i], true
		top := byte(0)
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case (c == ']' && top == '[' && (state == arrayFirst || state == next)) || (c == '}' && top == '{' && (state == objectFirst || state == next)):
			if cutDepth != 0 && len(stack) == cutDepth {
				out.WriteString(doc[copied:cutAt])
				out.WriteString("null")
				copied, cutDepth = i+1, 0
			}
			stack, state = stack[:len(stack)-1], next
			i++
		case c == ',' && state == next && top != 0:
			state = value
			if top == '{' {
				state = key
			}
			i++
		case c == ':' && state == colon:
			state = value
			i++
		case c == '"' && (state == key || state == objectFirst):
			i, ok = spawnHookRouteString(doc, i)
			state = colon
		case state == value || state == arrayFirst:
			if c == '[' || c == '{' {
				if stack = append(stack, c); len(stack) == spawnHookRouteMaxDepth+3 && cutDepth == 0 {
					cutDepth, cutAt = len(stack), i
				}
				state = arrayFirst
				if c == '{' {
					state = objectFirst
				}
				i++
			} else {
				if c == '"' {
					i, ok = spawnHookRouteString(doc, i)
				} else {
					i, ok = spawnHookRouteScalar(doc, i)
				}
				state = next
			}
		default:
			ok = false
		}
		if !ok {
			return "", false
		}
		if state == next && len(stack) == 0 { // the value is complete: only white space may follow
			if strings.Trim(doc[i:], " \t\n\r") != "" {
				return "", false
			}
			out.WriteString(doc[copied:])
			return out.String(), true
		}
	}
	return "", false
}

// spawnHookRouteString returns the index after the JSON string that opens at doc[i]: no raw control character, only the escapes of
// the grammar.
func spawnHookRouteString(doc string, i int) (int, bool) {
	for i++; i < len(doc); i++ {
		switch c := doc[i]; {
		case c == '"':
			return i + 1, true
		case c < 0x20:
			return 0, false
		case c == '\\':
			if i++; i >= len(doc) {
				return 0, false
			}
			switch doc[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if i+4 >= len(doc) || strings.ContainsFunc(doc[i+1:i+5], func(r rune) bool { return !strings.ContainsRune("0123456789abcdefABCDEF", r) }) {
					return 0, false
				}
				i += 4
			default:
				return 0, false
			}
		}
	}
	return 0, false
}

// spawnHookRouteScalar returns the index after the JSON literal or number that starts at doc[i].
func spawnHookRouteScalar(doc string, i int) (int, bool) {
	for _, literal := range []string{"true", "false", "null"} {
		if strings.HasPrefix(doc[i:], literal) {
			return i + len(literal), true
		}
	}
	digits := func(j int) int {
		for j < len(doc) && doc[j] >= '0' && doc[j] <= '9' {
			j++
		}
		return j
	}
	j := i
	if j < len(doc) && doc[j] == '-' {
		j++
	}
	switch end := digits(j); {
	case j < len(doc) && doc[j] == '0':
		j++
	case end > j && doc[j] != '0':
		j = end
	default:
		return 0, false
	}
	if j < len(doc) && doc[j] == '.' {
		if end := digits(j + 1); end > j+1 {
			j = end
		} else {
			return 0, false
		}
	}
	if j < len(doc) && (doc[j] == 'e' || doc[j] == 'E') {
		k := j + 1
		if k < len(doc) && (doc[k] == '+' || doc[k] == '-') {
			k++
		}
		if end := digits(k); end > k {
			j = end
		} else {
			return 0, false
		}
	}
	return j, true
}

// spawnHookRoute is :991-1115 without the managed and final-gate legs: routing, the message, the items, the notices and the envelope.
// The final-gate prerequisite check (:1072-1079) runs before the depth return, as the oracle's check runs before the JSON.stringify
// at :1103 whose RangeError a tool_input nested too deep causes; the depth judgement is computed first and only its result is
// applied after the gate, so a refusal wins and nothing computed before the gate recurses without bound over a deep tool_input.
func spawnHookRoute(a spawnHookAssembly, env host.LookupEnv) string {
	tooDeep := spawnHookRouteDeep(a.toolInput)
	prompt, model, effort := spawnHookRouteSettings(a)
	message := a.updatedMessage
	// A prompt that already follows the guard is not inserted again, in the single-message form as in the items form (CRW-1121;
	// the oracle checked the items form only, so a reapplied message got the prompt twice).
	if prompt != "" {
		forms := a.promptForms
		if forms == nil {
			forms = []string{prompt}
		}
		if lead, ok := strings.CutPrefix(message, a.guard+"\n\n"); !ok || !spawnHookPromptLeads(lead, forms) {
			message = spawnHookRoutePrompt(message, a.guard, prompt, a.validItems, a.v2Spawn)
		}
	}
	promptChanged := !a.encryptedV2Message && prompt != ""
	message = a.trustPrefix + message
	if a.encryptedV2Message {
		message = a.message // the native backend reads this whole value as ciphertext
	}
	var items []any
	changed := message != a.message || promptChanged
	if a.validItems {
		items = spawnHookRouteItems(a, message)
		if !tooDeep {
			changed = spawnHookRouteStringify(items) != spawnHookRouteStringify(a.itemInput)
		}
	}
	// Final-gate prerequisites (:1072-1079): the packet's text items joined, or the message, and the session. A refusal is the
	// deny envelope; every other path fails open. It runs before the depth return below so a denial reaches the caller, as the
	// oracle's check runs before the JSON.stringify at :1103. Only the item/input comparison recurses over the whole value, so it is
	// skipped for a tool_input nested past the depth limit; spawnHookRouteItems reads each item's type and text shallowly, so the
	// gate text is the same joined item text (or message) a shallow packet yields and the gate still decides. A deep packet the
	// gate refuses is denied, where the oracle's JSON.stringify throws before its own gate and its outer catch allows it: the gate
	// first and fail closed, as the issue answers.
	gateText := message
	if a.validItems {
		var texts []string
		for _, item := range items {
			if o, ok := item.(pyjson.Object); ok && o.Get("type") == "text" {
				texts = append(texts, o.Get("text").(string))
			}
		}
		gateText = strings.Join(texts, "\n\n")
	}
	if gate := CheckFinalGatePrereqs(gateText, a.sessionID, a.cwd, nil); !gate.OK {
		reason := gate.Reason
		if reason == "" {
			reason = "final gate prerequisites are missing"
		}
		return DenyEnvelope(reason)
	}
	if tooDeep {
		if a.managed == nil {
			return a.finish("", env) // the oracle's JSON.stringify throws a RangeError here, which its outer catch turns into nothing
		}
		// A managed spawn must run with the candidate the attempt was claimed for and be recorded as issued; an answer that cannot
		// be written would let the host run the caller's input instead, so it is denied before anything is issued (CRW-1122; the
		// oracle issued and then printed nothing).
		return DenyEnvelope("managed dispatch: " + spawnHookDeepReason)
	}
	routed, err := a.settings.Role(a.role) // the event's snapshot, as the role resolution read it (CRW-1124)
	if err != nil {
		if deny := spawnHookSettingsDeny(err); deny != "" {
			return deny
		}
		return a.finish("", env) // the oracle's throw, caught by its outer catch
	}
	var notices []string
	if a.managed == nil && routed.Fallback != nil {
		notices = append(notices, "[crw] This direct spawn is not managed by first-fallback tracking. For subsequent tasks: "+role.DispatchGuidance)
	}
	if a.encryptedV2Message {
		notices = append(notices, "[crw] Native V2 task ciphertext was preserved. Hook-added skill text, scope instructions and prompt overrides were not attached; native recursion checks and separate routing fields still apply.")
	}
	context := strings.Join(notices, "\n")
	if a.managed == nil && context == "" && !changed && model == "" && effort == "" {
		return a.finish("", env)
	}
	updated := slices.Clone(a.toolInput) // Set changes a present key in place, and a.toolInput is the caller's
	if a.validItems {
		updated = updated.Set("items", items)
	} else {
		updated = updated.Set("message", message)
	}
	if model != "" {
		updated = updated.Set("model", model)
	}
	if effort != "" {
		updated = updated.Set("reasoning_effort", effort)
	}
	if a.managed != nil {
		// The candidate's model and effort replace whatever the caller sent, a null candidate field deleting the key (:1098-1101).
		if a.managed.Candidate.Model == nil {
			updated = spawnHookWithout(updated, "model")
		} else {
			updated = updated.Set("model", *a.managed.Candidate.Model)
		}
		if a.managed.Candidate.Effort == nil {
			updated = spawnHookWithout(updated, "reasoning_effort")
		} else {
			updated = updated.Set("reasoning_effort", string(*a.managed.Candidate.Effort))
		}
	}
	// CRW-1115: the evidence assignment injected into the packet is recorded only now that the spawn is allowed, and before a
	// managed spawn is issued (CRW-1106): the issuance is a one-shot of the dispatch ledger, so a record that cannot be written
	// refuses the spawn while the attempt is still issuable, and the record is removed again when the issuance is refused.
	// A child told a location the gate does not know would be unverifiable, which is why a record that cannot be written denies.
	// The record carries the digest of the input this answer gives, so that input delivered again is known as this event (CRW-1121).
	if a.evidenceAssignment != nil && !a.evidenceRecorded {
		spawnHookBeforePersist()
		if a.inputText != "" {
			a.evidenceAssignment.AnswerInput = spawnHookDigest(spawnHookRouteStringify(updated))
		}
		// The record is created, never replaced: an event named after itself that another delivery registered first (and whose child
		// may have claimed it) keeps that record, and this delivery answers with it (CRW-1121, CRW-1124).
		if err := a.evidenceAssignment.PersistNew(a.cwd); errors.Is(err, evidence.ErrAssignmentExists) {
			a.evidenceRecorded = true
		} else if err != nil {
			return DenyEnvelope("evidence assignment: the record could not be written: " + err.Error())
		}
	}
	output := pyjson.Object{{Key: "hookEventName", Value: "PreToolUse"}, {Key: "permissionDecision", Value: "allow"}, {Key: "updatedInput", Value: updated}}
	if context != "" {
		output = append(output, pyjson.Field{Key: "additionalContext", Value: context})
	}
	answer := a.finish(spawnHookRouteStringify(pyjson.Object{{Key: "hookSpecificOutput", Value: output}})+"\n", env)
	if a.supersedes != nil && strings.Contains(answer, `"permissionDecision":"allow"`) {
		a.supersedes.Remove(a.cwd) // no child holds it and no packet names it any more (CRW-1121)
	}
	if a.replay != nil && strings.Contains(answer, `"permissionDecision":"allow"`) {
		a.replay(answer) // CRW-1121: the same event again gets this answer
	}
	return answer
}

// spawnHookBeforePersist runs before the route writes the event's evidence assignment; a test acts there.
var spawnHookBeforePersist = func() {}

// finish commits what an answer that lets the spawn run needs, after every refusal has been checked and the answer is written
// (CRW-1118, CRW-1122): a subagent's grant is reserved, the managed attempt is issued (:1094-1097), and the grant is spent. A
// refusal at a step denies and gives a reserved grant back; past the commits the answer is only returned, and a failure there
// is an unknown outcome (RunSpawnAttachHook), never the caller's own input and never a grant given back.
func (a spawnHookAssembly) finish(answer string, env host.LookupEnv) string {
	if a.grant != nil && !a.grant.reserve(time.Now()) {
		a.dropEvidenceAssignment()
		return DenyEnvelope(RecurseDenyReason) // another call took the grant first
	}
	if a.managed != nil {
		// The issuance works on the preview's root and attempt and re-reads the record under its lock only (CRW-1124).
		if _, err := role.IssueManagedSpawnSelection(a.managed, a.toolUseID, env); err != nil {
			if a.grant != nil {
				a.grant.release()
			}
			a.dropEvidenceAssignment()
			return DenyEnvelope("managed dispatch: " + spawnParityNodeError(err))
		}
		if a.commit != nil {
			a.commit.issued = true
		}
	}
	if a.grant != nil {
		// The call's input and answer are recorded before its grant is spent: a delivery that stopped between the two finds the
		// record and returns the answer, and one that never got there finds the reservation, held for this input (CRW-1118).
		if a.record != nil {
			a.record(answer)
		}
		a.grant.commit()
		if a.commit != nil {
			a.commit.granted = true
		}
	}
	return answer
}

// dropEvidenceAssignment takes back the evidence assignment the route recorded (CRW-1115) when finish refuses the spawn, so a child
// that never ran leaves no open assignment behind; the record was written before the issuance, which is a one-shot (CRW-1106).
func (a spawnHookAssembly) dropEvidenceAssignment() {
	if a.evidenceAssignment != nil && !a.evidenceRecorded { // a record an earlier delivery wrote stays with that delivery's child
		a.evidenceAssignment.Remove(a.cwd)
	}
}

// spawnHookDeepReason is why a managed spawn nested too deep is refused, with what the caller does about it.
var spawnHookDeepReason = "the spawn's tool_input nests deeper than " + strconv.Itoa(spawnHookRouteMaxDepth) +
	" levels, so the managed candidate cannot be applied; remove the deeply nested fields and spawn again"

// spawnHookBusyReason answers a root spawn event whose lock another delivery of the same event keeps: nothing is minted for it.
const spawnHookBusyReason = "crw: another delivery of this spawn call is still being answered; try again"

// spawnHookReconcileReason answers a spawn whose managed attempt was issued when the hook then failed to give its answer: the
// attempt is recorded as issued to this native call, so it is neither retried nor spawned again here.
const spawnHookReconcileReason = "managed dispatch: the attempt was issued but the hook could not answer; inspect the dispatch status and reconcile before retry"

// spawnHookCommit is what a hook run has committed, shared by RunSpawnAttachHook and the route so a failure after a commit is
// answered as an unknown outcome and never as an allow of the caller's own input.
type spawnHookCommit struct {
	subagent bool   // the spawner is a subagent, so nothing may let it through without its grant
	granted  bool   // the subagent's grant is spent
	issued   bool   // the managed attempt is issued
	unlock   func() // releases the lock of the event, when this run holds it (CRW-1121)
}

// spawnHookRouteSettings is :999-1016: the trimmed promptOverride, which no fork restricts, and the model and effort to inject, which
// a full-history fork (codex-rs rejects them there) and a value the caller picked (a string that is not blank) both rule out.
func spawnHookRouteSettings(a spawnHookAssembly) (prompt, model, effort string) {
	if p := a.resolution.PromptOverride; p != nil {
		prompt = text.Trim(*p)
	}
	if IsFullHistoryFork(spawnHookView(a.toolInput)) {
		return prompt, "", ""
	}
	if a.managed != nil {
		return prompt, "", "" // a managed spawn takes the candidate's model and effort (:1006, :1098-1101)
	}
	picked := func(key string) bool { s, ok := a.toolInput.Get(key).(string); return ok && text.Trim(s) != "" }
	if m := a.resolution.Model; m != nil && !a.resolution.UsesMainModel && !picked("model") {
		model = *m
	}
	if e := a.resolution.Effort; e != nil && !picked("reasoning_effort") {
		effort = string(*e)
	}
	return prompt, model, effort
}

// spawnHookRoutePrompt is :1021-1046: the prompt after the guard. A message that is only the guard becomes guard and prompt, in both
// forms (CRW-1121; the oracle did this for items only, so a bare guard message lost its prompt); otherwise the prompt goes after the
// first guard-and-blank-line as literal text (CRW-1121; the oracle's String.replace read the prompt's $$, $&, $` and $' as
// replacement patterns). The oracle's empty-guard branches (an existing guard marker's block, else a prefix) are ported, but the
// guard is never empty.
func spawnHookRoutePrompt(message, guard, prompt string, items, v2 bool) string {
	switch {
	case message == guard:
		return guard + "\n\n" + prompt
	case guard != "":
		return strings.Replace(message, guard+"\n\n", guard+"\n\n"+prompt+"\n\n", 1)
	}
	marker := ScopeGuardMarker
	if v2 {
		marker = LeafGuardMarker
	}
	at := strings.Index(message, marker)
	if at < 0 {
		return prompt + "\n\n" + message
	}
	if end := strings.Index(message[at:], "\n\n"); end >= 0 {
		return message[:at+end] + "\n\n" + prompt + message[at+end:]
	}
	return message + "\n\n" + prompt
}

// spawnHookPromptLeads reports whether s starts with the prompt in one of its forms.
func spawnHookPromptLeads(s string, forms []string) bool {
	_, ok := spawnHookPromptLead(s, forms)
	return ok
}

// spawnHookRouteItems is :1053-1064: the first text item takes the message (or a new text item goes first when there is none), and the
// skill blocks follow the last text item when the text items still total at most 256 KiB of UTF-16 units. Items are []any, which the
// writer reads (a []pyjson.Object would print as Go syntax).
func spawnHookRouteItems(a spawnHookAssembly, message string) []any {
	items := make([]any, len(a.mappedItems), len(a.mappedItems)+1)
	for i, item := range a.mappedItems {
		items[i] = item
	}
	if a.firstText < 0 {
		items = slices.Insert(items, 0, any(pyjson.Object{{Key: "type", Value: "text"}, {Key: "text", Value: message}}))
	} else {
		items[a.firstText] = slices.Clone(a.mappedItems[a.firstText]).Set("text", message)
	}
	if len(a.itemBlocks) == 0 {
		return items
	}
	var texts []int
	units := 0
	for i, item := range items {
		if o := item.(pyjson.Object); o.Get("type") == "text" {
			texts = append(texts, i)
			units += spawnInlineUTF16Units(o.Get("text").(string))
		}
	}
	suffix := "\n\n" + strings.Join(a.itemBlocks, "\n\n")
	if units+(len(texts)-1)*2+spawnInlineUTF16Units(suffix) <= spawnNormalizeMaxLength {
		last := items[texts[len(texts)-1]].(pyjson.Object)
		items[texts[len(texts)-1]] = slices.Clone(last).Set("text", last.Get("text").(string)+suffix)
	}
	return items
}

// spawnHookRouteStringify is JSON.stringify(v) for the values a payload holds: compact, only the quote, the backslash and the controls
// escaped, a lone surrogate written as its lowercase escape, never U+FFFD (encoding/json would write that), and the numbers and
// key order JavaScript's parse-then-stringify gives (spawnHookRouteJS).
func spawnHookRouteStringify(v any) string {
	return pyjson.Dumps(spawnHookRouteJS(v), pyjson.Options{Compact: true, Unicode: true, Bytes: pyjson.ReplacedBytes})
}

// spawnHookRouteJS is what a JavaScript object holds after JSON.parse, as JSON.stringify reads it: each number is the double it
// parses to, spelled by ECMAScript's Number::toString (1e21 is 1e+21, 1e-7 is 1e-7, a zero of either sign is 0, an infinity is null),
// and the keys that are canonical array indices come first in ascending order, the others in their order of appearance.
func spawnHookRouteJS(v any) any {
	switch v := v.(type) {
	case string:
		return spawnHookRouteJoin(v)
	case pyjson.Object:
		var indexed, named pyjson.Object
		for _, f := range v {
			f.Key, f.Value = spawnHookRouteJoin(f.Key), spawnHookRouteJS(f.Value)
			if _, ok := spawnHookRouteIndex(f.Key); ok {
				indexed = append(indexed, f)
			} else {
				named = append(named, f)
			}
		}
		slices.SortStableFunc(indexed, func(x, y pyjson.Field) int {
			a, _ := spawnHookRouteIndex(x.Key)
			b, _ := spawnHookRouteIndex(y.Key)
			return cmp.Compare(a, b)
		})
		return append(indexed, named...)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = spawnHookRouteJS(v[i])
		}
		return out
	case json.Number:
		f, _ := strconv.ParseFloat(string(v), 64)
		switch {
		case math.IsInf(f, 0):
			return nil
		case f == 0:
			return json.Number("0")
		}
		spelled, _ := json.Marshal(f) // encoding/json's float encoder is ECMAScript's Number::toString
		return json.Number(spelled)
	}
	return v
}

// spawnHookRouteJoin writes a high surrogate that a low surrogate follows as the one character a JavaScript string holds there. The hook
// strips control markers, which can bring the halves of a pair together, and a Go string keeps the two WTF-8 sequences apart: a pair of
// them would be written as two escapes, where JSON.stringify writes the character.
func spawnHookRouteJoin(s string) string {
	var joined []byte
	for i := 0; i < len(s); {
		if i+5 < len(s) && s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xaf && s[i+2] >= 0x80 && s[i+2] <= 0xbf &&
			s[i+3] == 0xed && s[i+4] >= 0xb0 && s[i+4] <= 0xbf && s[i+5] >= 0x80 && s[i+5] <= 0xbf {
			high, _ := pyjson.CodePoint(s, i)
			low, _ := pyjson.CodePoint(s, i+3)
			if joined == nil {
				joined = []byte(s[:i])
			}
			joined = utf8.AppendRune(joined, 0x10000+(high-0xd800)<<10+(low-0xdc00))
			i += 6
			continue
		}
		if joined != nil {
			joined = append(joined, s[i])
		}
		i++
	}
	if joined == nil {
		return s
	}
	return string(joined)
}

// spawnHookRouteIndex reports whether key is an array index (a canonical uint32 below 2^32-1), the keys a JavaScript object lists first.
func spawnHookRouteIndex(key string) (uint64, bool) {
	n, err := strconv.ParseUint(key, 10, 32)
	return n, err == nil && n != 1<<32-1 && strconv.FormatUint(n, 10) == key
}

// spawnHookRouteDeep reports whether v nests past spawnHookRouteMaxDepth levels, counting with an explicit stack. The oracle's
// JSON.parse is iterative, so a payload nested deeper than that still parses, and a subagent spawn that carries such a field must reach
// the recursion deny; only writing the answer fails there. pyjson.Dumps and spawnHookRouteJS recurse once per level, and an overflow
// of the goroutine stack ends the process where recover cannot help, so the answer is written only for a bounded value.
func spawnHookRouteDeep(v any) bool {
	type node struct {
		v     any
		depth int
	}
	stack := []node{{v, 1}}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		var children []any
		switch c := n.v.(type) {
		case pyjson.Object:
			for _, f := range c {
				children = append(children, f.Value)
			}
		case []any:
			children = c
		case map[string]any:
			for _, x := range c {
				children = append(children, x)
			}
		default:
			continue
		}
		if n.depth > spawnHookRouteMaxDepth {
			return true
		}
		for _, x := range children {
			stack = append(stack, node{x, n.depth + 1})
		}
	}
	return false
}
