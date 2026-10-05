package spawn

import (
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// This file ports the second half of CXC v0.2.40's runSpawnAttachHook (subagent-config/src/spawn-attach-hook.ts:849-852 and :991-1115:
// the 4 MiB input bound, role routing, the ciphertext restore, the item re-assembly, the notices and the output envelope) over the
// assembly of hook.go, with the CRW names of contract/schema/cxc/name-substitution.json. It leaves out what a later issue owns, so
// "managed" is always absent: the managed dispatch loop and its candidate model and effort (:889-907, :1075-1114) and the final-gate
// check (:1066-1073). Nothing here registers, or is, a hook. Differences from the oracle, each recorded in docs/port-cxc/known-defects.md:
//   - the project layer is dropped (decision 7), so the trust warning is empty: trustPrefix is applied, but nothing sets it;
//   - the oracle's :1030-1046 branches for an empty guard are ported in spawnHookRoutePrompt, but the guard is never empty;
//   - a value nested past 4,400 levels prints nothing, where the oracle's JSON.stringify fails near 4,458 at Node 24's default stack
//     (the threshold depends on the stack size); the payload is still parsed at any depth, as JSON.parse does, so a subagent's
//     recursion is still denied.

const (
	spawnHookRouteMaxInput = 4 * 1024 * 1024
	spawnHookRouteMaxDepth = 4400 // V8's JSON.stringify fails near 4,458 levels at Node 24's default stack
)

// RunSpawnAttachHook is runSpawnAttachHook: the hook output for one PreToolUse payload, "" to allow it untouched, a deny envelope, or
// an allow envelope whose updatedInput replaces tool_input. It is total: a payload that does not parse, or a panic, prints nothing.
// raw is the stdin text already decoded as Node decodes it, so len(raw) is Buffer.byteLength(raw).
func RunSpawnAttachHook(raw string, env host.LookupEnv) (out string) {
	if len(raw) > spawnHookRouteMaxInput {
		return DenyEnvelope("crw spawn policy input exceeded 4 MiB; refusing to bypass the recursion and trust boundary")
	}
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	payload, ok := spawnHookRouteLoad(raw)
	if !ok {
		return ""
	}
	a, deny, stop := spawnHookAssemble(spawnHookView(payload), env)
	if stop {
		return deny
	}
	return spawnHookRoute(a, env)
}

// spawnHookRouteLoad is JSON.parse((raw ?? "").trim() || "{}") and isRecord: an ordered object, or false. A lone surrogate escape is
// kept, as is a number's spelling (the respelling happens when the answer is written).
func spawnHookRouteLoad(raw string) (pyjson.Object, bool) {
	doc := text.Trim(raw)
	if doc == "" {
		doc = "{}"
	}
	v, err := pyjson.Loads(doc, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	payload, ok := v.(pyjson.Object)
	return payload, err == nil && ok
}

// spawnHookRoute is :991-1115 without the managed and final-gate legs: routing, the message, the items, the notices and the envelope.
func spawnHookRoute(a spawnHookAssembly, env host.LookupEnv) string {
	if spawnHookRouteDeep(a.toolInput) {
		return "" // the oracle's JSON.stringify throws a RangeError here, which its outer catch turns into nothing
	}
	prompt, model, effort := spawnHookRouteSettings(a)
	message := a.updatedMessage
	if prompt != "" && !(a.validItems && (message == a.guard+"\n\n"+prompt || strings.HasPrefix(message, a.guard+"\n\n"+prompt+"\n\n"))) {
		message = spawnHookRoutePrompt(message, a.guard, prompt, a.validItems, a.v2Spawn)
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
		changed = spawnHookRouteStringify(items) != spawnHookRouteStringify(a.itemInput)
	}
	config, err := role.ReadConfig(env)
	if err != nil {
		return "" // the oracle's throw, caught by its outer catch
	}
	var notices []string
	if config.Roles[a.role].Fallback != nil {
		notices = append(notices, "[crw] This direct spawn is not managed by first-fallback tracking. For subsequent tasks: "+role.DispatchGuidance)
	}
	if a.encryptedV2Message {
		notices = append(notices, "[crw] Native V2 task ciphertext was preserved. Hook-added skill text, scope instructions and prompt overrides were not attached; native recursion checks and separate routing fields still apply.")
	}
	context := strings.Join(notices, "\n")
	if context == "" && !changed && model == "" && effort == "" {
		return ""
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
	output := pyjson.Object{{Key: "hookEventName", Value: "PreToolUse"}, {Key: "permissionDecision", Value: "allow"}, {Key: "updatedInput", Value: updated}}
	if context != "" {
		output = append(output, pyjson.Field{Key: "additionalContext", Value: context})
	}
	return spawnHookRouteStringify(pyjson.Object{{Key: "hookSpecificOutput", Value: output}}) + "\n"
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
	picked := func(key string) bool { s, ok := a.toolInput.Get(key).(string); return ok && text.Trim(s) != "" }
	if m := a.resolution.Model; m != nil && !a.resolution.UsesMainModel && !picked("model") {
		model = *m
	}
	if e := a.resolution.Effort; e != nil && !picked("reasoning_effort") {
		effort = string(*e)
	}
	return prompt, model, effort
}

// spawnHookRoutePrompt is :1021-1046: the prompt after the guard. A message that is only the guard (items) becomes guard and prompt;
// otherwise the first guard-and-blank-line is replaced, as String.replace does with its $ patterns. The oracle's empty-guard branches
// (an existing guard marker's block, else a prefix) are ported, but the guard is never empty.
func spawnHookRoutePrompt(message, guard, prompt string, items, v2 bool) string {
	switch {
	case items && message == guard:
		return guard + "\n\n" + prompt
	case guard != "":
		return spawnHookRouteReplace(message, guard+"\n\n", guard+"\n\n"+prompt+"\n\n")
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

// spawnHookRouteReplace is s.replace(search, replacement) for a string search: the first occurrence only, with the replacement's $$,
// $&, $` and $' patterns expanded. A $n or $<name> stays as written: a string search has no captures.
func spawnHookRouteReplace(s, search, replacement string) string {
	at := strings.Index(s, search)
	if at < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(replacement); i++ {
		if replacement[i] != '$' || i+1 == len(replacement) {
			b.WriteByte(replacement[i])
			continue
		}
		switch replacement[i+1] {
		case '$':
			b.WriteByte('$')
		case '&':
			b.WriteString(search)
		case '`':
			b.WriteString(s[:at])
		case '\'':
			b.WriteString(s[at+len(search):])
		default:
			b.WriteByte('$')
			continue
		}
		i++
	}
	return s[:at] + b.String() + s[at+len(search):]
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
	case pyjson.Object:
		var indexed, named pyjson.Object
		for _, f := range v {
			f.Value = spawnHookRouteJS(f.Value)
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
