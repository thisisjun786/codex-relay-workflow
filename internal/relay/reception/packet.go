// Package reception validates typed relay packets against the receiver's own reading.
package reception

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type Obj = contract.OrderedObject

func O(pairs ...any) Obj {
	o := make(Obj, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		o = append(o, contract.Field{Key: pairs[i].(string), Value: pairs[i+1]})
	}
	return o
}
func Get(v any, k string) any {
	o, _ := evidence.Object(v)
	for _, f := range o {
		if f.Key == k {
			return f.Value
		}
	}
	return nil
}
func Has(v any, k string) bool {
	o, _ := evidence.Object(v)
	for _, f := range o {
		if f.Key == k {
			return true
		}
	}
	return false
}
func Set(o *Obj, k string, v any) {
	for i := range *o {
		if (*o)[i].Key == k {
			(*o)[i].Value = v
			return
		}
	}
	*o = append(*o, contract.Field{Key: k, Value: v})
}
func str(v any) string { s, _ := v.(string); return s }
func truth(v any) bool {
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case bool:
		return x
	case []any:
		return len(x) > 0
	case Obj:
		return len(x) > 0
	}
	return true
}
func absent(v any) bool {
	return slices.Contains([]any{"unknown", "inherited", "not_applicable"}, Get(v, "absent"))
}
func present(v any) bool {
	if v == nil || absent(v) {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case []any:
		return len(x) > 0
	case Obj:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}
func shown(v any) string {
	if absent(v) {
		if d := Get(v, "detail"); truth(d) {
			return "<" + str(Get(v, "absent")) + ": " + str(d) + ">"
		}
		return "<" + str(Get(v, "absent")) + ">"
	}
	if v == nil {
		return ""
	}
	return pyvalue.Str(v)
}
func malformed(format string, args ...any) error {
	return &store.RefusedError{Reason: "malformed_receipt", Detail: fmt.Sprintf(format, args...)}
}
func digest(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

var roles = map[string][2]string{"parent_to_child": {"parent", "child"}, "child_to_parent": {"child", "parent"}, "parent_to_supervisor": {"parent", "supervisor"}, "supervisor_to_parent": {"supervisor", "parent"}}
var purposes = map[string]map[string]string{
	"parent_to_child":      {"assignment": "request", "revision_request": "request", "resume": "request", "receipt_confirmation": "notification", "acceptance": "notification", "integration_result": "notification"},
	"child_to_parent":      {"completion": "request", "review_ready": "request", "blocked": "request", "progress": "notification", "decision_request": "decision"},
	"parent_to_supervisor": {"completion": "notification", "blocked": "notification", "decision_request": "decision", "fault_notice": "notification", "fault_decision": "decision", "status_response": "status_response"},
	"supervisor_to_parent": {"project_assignment": "request", "midpoint_check": "request", "resume": "request", "scope_correction": "request", "relayed_decision": "request", "user_stop": "request"},
}
var required = map[string][]string{
	"parent_to_child/assignment": {"issue", "criteriaDigest", "policy", "callback", "body"}, "parent_to_child/revision_request": {"issue", "generation", "criteriaDigest", "callback", "artifact", "body", "evidence"}, "parent_to_child/resume": {"issue", "policy", "callback", "artifact"}, "parent_to_child/receipt_confirmation": {"issue", "correlationId"}, "parent_to_child/acceptance": {"issue", "criteriaDigest", "artifact"}, "parent_to_child/integration_result": {"issue", "artifact"},
	"child_to_parent/completion": {"issue", "generation", "criteriaDigest", "artifact", "evidence"}, "child_to_parent/review_ready": {"issue", "generation", "criteriaDigest", "artifact", "evidence"}, "child_to_parent/blocked": {"issue", "evidence"}, "child_to_parent/decision_request": {"issue", "decision", "evidence"}, "child_to_parent/progress": {"issue"},
	"parent_to_supervisor/completion": {"issue", "generation", "evidence"}, "parent_to_supervisor/blocked": {"issue", "evidence"}, "parent_to_supervisor/decision_request": {"issue", "decision", "evidence"}, "parent_to_supervisor/fault_notice": {"evidence"}, "parent_to_supervisor/fault_decision": {"decision", "evidence"},
}

func known(v any, allowed []string, what string) error {
	if slices.Contains(allowed, str(v)) && str(v) != "" {
		return nil
	}
	sorted := slices.Clone(allowed)
	slices.Sort(sorted)
	return malformed("%s is not a known %s; it is one of %s", pyvalue.Repr(v), what, strings.Join(sorted, ", "))
}
func KindOf(direction, purpose any) (string, error) {
	if e := known(direction, []string{"child_to_parent", "parent_to_child", "supervisor_to_parent", "parent_to_supervisor"}, "direction"); e != nil {
		return "", e
	}
	p := purposes[str(direction)]
	kind, ok := p[str(purpose)]
	if !ok {
		names := []string{}
		for k := range p {
			names = append(names, k)
		}
		slices.Sort(names)
		return "", malformed("%s is not a purpose %s carries; it has %s", pyvalue.Repr(purpose), direction, strings.Join(names, ", "))
	}
	return kind, nil
}
func RequiredFor(direction, purpose string) ([]string, error) {
	if _, e := KindOf(direction, purpose); e != nil {
		return nil, e
	}
	r, ok := required[direction+"/"+purpose]
	if !ok {
		return nil, malformed("relay-packet/1 covers the parent-child relation and what a parent owes upward; %s is carried by the envelope alone", direction)
	}
	return slices.Clone(r), nil
}
func MessageID(direction, relation, purpose, subject string) (string, error) {
	if _, e := KindOf(direction, purpose); e != nil {
		return "", e
	}
	for _, f := range []struct{ k, v string }{{"relation_id", relation}, {"subject", subject}} {
		if strings.TrimSpace(f.v) == "" {
			return "", malformed("%s must be a non-empty string", f.k)
		}
		if strings.Contains(f.v, "|") {
			return "", malformed("%s must not contain '|', which is the field separator", f.k)
		}
	}
	return digest(direction + "|" + relation + "|" + purpose + "|" + subject)[:32], nil
}

var reachNames = []string{"transport_accepted", "received", "agreed", "applied", "verified"}
var reachSources = map[string][]string{"child_to_parent": {"attempts", "acks", "acks", "verdicts", "verdicts"}, "parent_to_child": {"attempts", "", "", "events", "verdicts"}, "parent_to_supervisor": {"supervisor_attempts", "supervisor_readbacks", "", "", ""}, "supervisor_to_parent": {"", "scope_directives", "scope_directives", "", ""}}
var noMechanism = map[string]string{"parent_to_child": "a revision request carries no acknowledgement; the completion receipt of the generation it opened is what shows it was applied", "parent_to_supervisor": "a report upward is transported and read back, and nothing here records that a supervisor agreed, applied or verified anything; what became of it is read from the Linear record, confirmed", "supervisor_to_parent": "a directive is recorded rather than sent, so nothing transports, applies or verifies this one"}
var states = []string{"yes", "no", "conditional", "unmeasured", "not_applicable"}

func Stage(state string, source any, detail string) Obj {
	return O("state", state, "source", source, "detail", detail)
}
func checkEnvelope(region any) error {
	for _, k := range []string{"direction", "kind", "purpose"} {
		if _, ok := Get(region, k).(string); !ok {
			return malformed("the %s is a name, not a %s", k, pyvalue.TypeName(Get(region, k)))
		}
	}
	kind := str(Get(region, "kind"))
	if e := known(kind, []string{"request", "notification", "decision", "status_response"}, "kind"); e != nil {
		return e
	}
	keys := []string{"version", "direction", "kind", "purpose", "messageId", "relationId", "sender", "recipient"}
	keys = append(keys, map[string][]string{"request": {"subject", "answerOwedBy"}, "decision": {"subject", "answerOwedBy", "decision"}, "notification": {"subject"}, "status_response": {"correlationId"}}[kind]...)
	for _, k := range keys {
		v := Get(region, k)
		if v == nil || absent(v) || pyvalue.TypeName(v) == "str" && strings.TrimSpace(str(v)) == "" {
			return malformed("a %s envelope cannot omit %s: %s", kind, k, shown(v))
		}
	}
	for _, k := range []string{"sender", "recipient"} {
		v := Get(region, k)
		if _, ok := evidence.Object(v); !ok {
			return malformed("the %s is an endpoint object with a task id, not a %s", k, pyvalue.TypeName(v))
		}
		task := Get(v, "taskId")
		if !absent(task) && (pyvalue.TypeName(task) != "str" || strings.TrimSpace(str(task)) == "") {
			return malformed("the %s is neither a task id nor a stated absence", k)
		}
	}
	reach := Get(region, "reach")
	if reach != nil {
		if _, ok := evidence.Object(reach); !ok {
			return malformed("the reach is a ladder of named stages, not a %s", pyvalue.TypeName(reach))
		}
	}
	direction := str(Get(region, "direction"))
	if e := known(direction, []string{"child_to_parent", "parent_to_child", "supervisor_to_parent", "parent_to_supervisor"}, "direction"); e != nil {
		return e
	}
	for i, n := range reachNames {
		if !Has(reach, n) {
			return malformed("a reach ladder answers every stage; %s is missing. Start from unreached(direction) rather than from a partial dictionary", n)
		}
		entry := Get(reach, n)
		state := Get(entry, "state")
		if e := known(state, states, "reach state"); e != nil {
			return e
		}
		source := reachSources[direction][i]
		if source == "" {
			if state != "not_applicable" {
				return malformed("%s has no mechanism for %s, so it cannot answer %s: %s", direction, n, pyvalue.Repr(state), noMechanism[direction])
			}
		} else if slices.Contains([]any{"yes", "no", "conditional"}, state) && Get(entry, "source") != source {
			return malformed("%s on %s is answered by %s, not by %s", n, direction, source, pyvalue.Repr(Get(entry, "source")))
		}
	}
	return nil
}
func Check(one any) error {
	if _, ok := evidence.Object(one); !ok {
		return malformed("a packet is an object with an envelope and its typed data, not a %s", pyvalue.TypeName(one))
	}
	region := Get(one, "envelope")
	if _, ok := evidence.Object(region); !ok {
		return malformed("a packet carries a relay-envelope/1 region under envelope, not a %s", pyvalue.TypeName(region))
	}
	if Get(one, "version") != "relay-packet/1" {
		return malformed("this reader is relay-packet/1 and the packet says %s; a version nobody mapped is diagnosed rather than read under these rules", pyvalue.Repr(Get(one, "version")))
	}
	if e := checkEnvelope(region); e != nil {
		return e
	}
	if Get(region, "version") != "relay-envelope/1" {
		return malformed("this reader is relay-envelope/1 and the region says %s; the identification region is read under the version that wrote it or not at all", pyvalue.Repr(Get(region, "version")))
	}
	direction, purpose := str(Get(region, "direction")), str(Get(region, "purpose"))
	kind, e := KindOf(direction, purpose)
	if e != nil {
		return e
	}
	if Get(region, "kind") != kind {
		return malformed("this region says it is a %s, but %s/%s is a %s; the kind is what the recipient owes, and it is derived rather than declared", pyvalue.Repr(Get(region, "kind")), direction, purpose, kind)
	}
	for i, k := range []string{"sender", "recipient"} {
		if Get(Get(region, k), "role") != roles[direction][i] {
			return malformed("the %s claims the role %s, but on %s it is the %s; a direction fixes both roles and a caller supplies neither", k, pyvalue.Repr(Get(Get(region, k), "role")), direction, roles[direction][i])
		}
	}
	id, e := MessageID(direction, str(Get(region, "relationId")), purpose, str(Get(region, "subject")))
	if e != nil {
		return e
	}
	if Get(region, "messageId") != id {
		return malformed("this region carries messageId %s, but its own direction, relation, purpose and subject derive %s; the identifier belongs to another message", pyvalue.Repr(Get(region, "messageId")), id)
	}
	for _, k := range []string{"relationId", "messageId", "subject", "correlationId", "relationRevision", "decision"} {
		v := Get(region, k)
		if v == nil || absent(v) {
			continue
		}
		want := "str"
		if k == "relationRevision" {
			want = "int"
		}
		if pyvalue.TypeName(v) != want {
			return malformed("the region's %s is %s or a stated absence, not a %s", k, want, pyvalue.TypeName(v))
		}
	}
	req, e := RequiredFor(direction, purpose)
	if e != nil {
		return e
	}
	for _, k := range req {
		v := Get(one, k)
		if k == "correlationId" || k == "decision" {
			v = Get(region, k)
		}
		if !present(v) {
			return malformed("a %s packet cannot omit %s: %s. It is one of %s, which this occasion is read against", purpose, k, shown(v), strings.Join(req, ", "))
		}
	}
	for _, field := range []struct{ k, want string }{{"issue", "str"}, {"generation", "int"}, {"criteriaDigest", "str"}, {"callback", "dict"}, {"body", "str"}} {
		v := Get(one, field.k)
		if v != nil && pyvalue.TypeName(v) != field.want {
			return malformed("%s is %s, not a %s; a value of the wrong shape compares equal to an equally wrong record value and comes back agreed", field.k, field.want, pyvalue.TypeName(v))
		}
	}
	if v := Get(one, "evidence"); v != nil {
		items, ok := evidence.List(v)
		if !ok {
			return malformed("evidence is a list of pointers, not a %s", pyvalue.TypeName(v))
		}
		for _, item := range items {
			if pyvalue.TypeName(item) != "str" || strings.TrimSpace(str(item)) == "" {
				return malformed("each evidence entry is a pointer somebody can follow, not %s", pyvalue.Repr(item))
			}
		}
	}
	if p := Get(one, "policy"); p != nil {
		if _, ok := evidence.Object(p); !ok {
			return malformed("a policy is an object of named settings, not a %s", pyvalue.TypeName(p))
		}
		missing := []string{}
		for _, k := range []string{"model", "effort", "workflow", "mode"} {
			if !present(Get(p, k)) {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return &store.RefusedError{Reason: "settings_incomplete", Detail: "the policy states " + strings.Join(missing, ", ") + " as nothing; the workflow in particular has no transport field, so an unstated one is dropped rather than defaulted"}
		}
		wrong := []string{}
		for _, k := range []string{"model", "effort", "workflow", "approval"} {
			if Get(p, k) != nil && pyvalue.TypeName(Get(p, k)) != "str" {
				wrong = append(wrong, k)
			}
		}
		if sandbox := Get(p, "sandbox"); sandbox != nil && pyvalue.TypeName(sandbox) != "str" && registry.NormalisePolicy(sandbox) == nil {
			wrong = append(wrong, "sandbox")
		}
		if len(wrong) > 0 {
			return malformed("the policy states %s as something other than text (a sandbox may also be a policy object naming its type); a setting is a name, and another shape would agree with its own spelling in the record", strings.Join(wrong, ", "))
		}
		if e := checkMode(Get(p, "mode")); e != nil {
			return e
		}
		words := strings.FieldsFunc(strings.ToLower(str(Get(p, "workflow"))), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		for i := 0; i+1 < len(words); i++ {
			if words[i] == "cxc" && words[i+1] == "loop" && Get(p, "mode") != "loop" {
				return malformed("the workflow names CXC Loop and the policy says %s; the Loop arms a goalplan, so its mode is loop", pyvalue.Repr(Get(p, "mode")))
			}
		}
	}
	if c := Get(one, "callback"); c != nil {
		wrong := []string{}
		for _, k := range []string{"taskId", "model", "effort"} {
			if pyvalue.TypeName(Get(c, k)) != "str" || !present(Get(c, k)) {
				wrong = append(wrong, k)
			}
		}
		co, _ := evidence.Object(c)
		extra := []string{}
		for _, f := range co {
			if !slices.Contains([]string{"taskId", "model", "effort"}, f.Key) {
				extra = append(extra, f.Key)
			}
		}
		slices.Sort(extra)
		for _, k := range extra {
			wrong = append(wrong, "unexpected "+k)
		}
		if len(wrong) > 0 {
			return malformed("the callback states %s; it is the task to answer and the model and effort that task runs now, each as text, and nothing else", strings.Join(wrong, ", "))
		}
	}
	if artifact := Get(one, "artifact"); artifact != nil {
		if e := checkArtifact(artifact); e != nil {
			return e
		}
	}
	if body := Get(one, "body"); present(body) {
		sections, form := DispatchSections, "DISPATCH-TASK-01"
		if direction == "parent_to_child" && purpose == "revision_request" {
			sections, form = CorrectionSections, "the correction form"
		}
		if missing := SectionProblems(body, sections); len(missing) > 0 {
			return malformed("the instruction body is missing %s; %s fixes these sections because an instruction that omits one is an instruction the recipient has to guess at", strings.Join(missing, ", "), form)
		}
	}
	if triple := Get(one, "activation"); triple != nil {
		if _, ok := evidence.Object(triple); !ok {
			return malformed("an activation reading is an object of three named facts, not a %s", pyvalue.TypeName(triple))
		}
		for _, k := range activationFacts {
			if !Has(triple, k) {
				return malformed("an activation reading answers all three facts; %s is missing", k)
			}
			fact := Get(triple, k)
			if _, ok := evidence.Object(fact); !ok {
				return malformed("the %s activation fact is an object with a state, not a %s", k, pyvalue.TypeName(fact))
			}
			if _, e := ActivationFact(Get(fact, "state"), Get(fact, "source"), str(Get(fact, "detail"))); e != nil {
				return e
			}
		}
		mode := Get(triple, "mode")
		if !slices.Contains(modes, str(mode)) {
			return malformed("an activation reading states the mode it was read under, one of coordination, loop, non_loop, not %s; not_applicable means something only under a mode that arms nothing", pyvalue.Repr(mode))
		}
		if _, e := ActivationClass(triple, str(mode), nil); e != nil {
			return e
		}
		if p := Get(one, "policy"); present(p) && Get(p, "mode") != mode {
			return malformed("the activation reading was taken under mode %s and the policy this packet states runs under %s; one packet cannot say both, and an audit's not_applicable is not a loop child's", pyvalue.Repr(mode), pyvalue.Repr(Get(p, "mode")))
		}
	}
	return nil
}
func checkArtifact(a any) error {
	kind := str(Get(a, "kind"))
	if !slices.Contains([]string{"pull_request", "locator"}, kind) {
		return malformed("an artifact is a pull request or a locator; a third shape cannot reach a reader as either")
	}
	req := []string{"path", "digest"}
	texts, optional := []string{"path", "digest"}, []string{"producedAt"}
	if kind == "pull_request" {
		req = []string{"repository", "number", "headSha"}
		texts, optional = []string{"repository", "headSha"}, []string{"baseSha", "url"}
	}
	missing := []string{}
	for _, k := range req {
		if !present(Get(a, k)) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return malformed("a %s artifact states %s; a deliverable identified by half its identity is not identified", kind, strings.Join(missing, ", "))
	}
	if kind == "pull_request" {
		n, ok := evidence.PyInt(Get(a, "number"))
		if !ok || n < 1 {
			return malformed("a pull request number is a positive integer, not a %s", pyvalue.TypeName(Get(a, "number")))
		}
	}
	wrong := []string{}
	types := []string{}
	for _, k := range append(texts, optional...) {
		v := Get(a, k)
		if v == nil && slices.Contains(optional, k) {
			continue
		}
		if pyvalue.TypeName(v) != "str" {
			wrong = append(wrong, k)
			types = append(types, pyvalue.TypeName(v))
		}
	}
	if len(wrong) > 0 {
		return malformed("a %s artifact states %s as text, not as %s", kind, strings.Join(wrong, ", "), strings.Join(types, ", "))
	}
	return nil
}

var DispatchSections = []string{"TASK", "SCOPE", "MUST DO", "MUST NOT", "PROOF", "RETURN FORMAT", "DECISION BOUNDARY"}
var CorrectionSections = []string{"VIOLATED CRITERION", "WHAT CHANGED", "FIX SCOPE", "PRESERVE", "REVERIFY AND RETURN"}

func SectionProblems(body any, sections []string) []string {
	source, ok := body.(string)
	if !ok {
		return slices.Clone(sections)
	}
	seen := map[string]bool{}
	current := ""
	for _, line := range strings.Split(strings.ReplaceAll(source, "\r", "\n"), "\n") {
		stripped := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*#>"))
		heading := strings.ToUpper(strings.Trim(stripped, "*`_"))
		opened := ""
		for _, s := range sections {
			if strings.HasPrefix(heading, s+":") || heading == s {
				opened = s
				break
			}
		}
		if opened != "" {
			if rest := strings.TrimSpace(strings.TrimLeft(heading[len(opened):], ":")); rest != "" {
				seen[opened] = true
				current = ""
			} else {
				current = opened
			}
			continue
		}
		if current != "" && stripped != "" {
			seen[current] = true
			current = ""
		}
	}
	out := []string{}
	for _, s := range sections {
		if !seen[s] {
			out = append(out, s)
		}
	}
	return out
}
func equal(a, b any) bool {
	return reflect.DeepEqual(a, b) || pyjson.Dumps(a, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) == pyjson.Dumps(b, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
}
