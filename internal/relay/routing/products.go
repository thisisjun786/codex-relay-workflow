// Package routing implements product routing's validated readings and pure decisions.
package routing

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	// The shared Python value helpers live here at the todo-22 base revision.
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

const ProjectsScope = "__projects__"

var Surfaces = []string{"dev_run", "verification", "user_report", "real_use"}
var Checks = []string{"acceptance", "install", "realUse", "handoff"}
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
var productPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var issuePattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

type Object = map[string]any

// Refusal preserves the routing reason separately from its caller-visible detail.
type Refusal struct{ Reason, Detail string }

func (e *Refusal) Error() string    { return e.Reason + ": " + e.Detail }
func malformed(detail string) error { return &Refusal{"route_input_malformed", detail} }

type validator struct{ err error }

func (v *validator) fail(detail string) {
	if v.err == nil {
		v.err = malformed(detail)
	}
}
func (v *validator) closed(value any, keys []string, name string) Object {
	m, ok := value.(map[string]any)
	if !ok {
		v.fail(name + " is an object")
		return Object{}
	}
	unknown := []string{}
	for k := range m {
		if !slices.Contains(keys, k) {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	if len(unknown) > 0 {
		v.fail(fmt.Sprintf("%s carries keys this contract does not define: %s", name, pyvalue.Repr(unknown)))
	}
	return m
}
func (v *validator) object(value any, schema string, keys []string, name string) Object {
	m, ok := value.(map[string]any)
	if !ok {
		v.fail(name + " is an object, not " + pyvalue.TypeName(value))
		return Object{}
	}
	if m["schema"] != schema {
		v.fail(fmt.Sprintf("%s carries schema %s, not %s", name, pyvalue.Repr(m["schema"]), schema))
	}
	return v.closed(m, keys, name)
}
func (v *validator) text(value any, name string, optional bool, limit int) any {
	if value == nil && optional {
		return nil
	}
	s, ok := value.(string)
	if !ok || strings.TrimSpace(s) == "" {
		v.fail(name + " is a non-blank string")
		return nil
	}
	if utf8.RuneCountInString(s) > limit {
		v.fail(fmt.Sprintf("%s is longer than %d characters", name, limit))
	}
	return strings.TrimSpace(s)
}
func (v *validator) project(value any, name string, optional bool) any {
	p := v.text(value, name, optional, 600)
	if p == ProjectsScope {
		v.fail(fmt.Sprintf("%s %s is reserved for routing's own project-create records", name, pyvalue.Repr(p)))
	}
	return p
}
func (v *validator) key(value any, name string, optional bool) any {
	if value == nil && optional {
		return nil
	}
	s, ok := value.(string)
	if !ok || !keyPattern.MatchString(s) {
		v.fail(fmt.Sprintf("%s is a key (%s), not %s", name, keyPattern.String(), pyvalue.Repr(value)))
	}
	return value
}
func (v *validator) product(value any, name string, optional bool) any {
	if value == nil && optional {
		return nil
	}
	s, ok := value.(string)
	if !ok || !productPattern.MatchString(s) {
		v.fail(fmt.Sprintf("%s is a plain product identifier (%s), not %s", name, productPattern.String(), pyvalue.Repr(value)))
	}
	return value
}
func (v *validator) issue(value any, name string, optional bool) any {
	if value == nil && optional {
		return nil
	}
	s, ok := value.(string)
	if !ok || !issuePattern.MatchString(s) {
		v.fail(fmt.Sprintf("%s is a Linear issue identifier like ABC-12, not %s", name, pyvalue.Repr(value)))
	}
	return value
}
func (v *validator) flag(value any, name string, defaultValue any) any {
	if value == nil && defaultValue != nil {
		return defaultValue
	}
	if _, ok := value.(bool); !ok {
		v.fail(name + " is a boolean")
	}
	return value
}
func tuple(values []string) string {
	parts := make([]string, len(values))
	for i, s := range values {
		parts[i] = pyvalue.Repr(s)
	}
	suffix := ""
	if len(values) == 1 {
		suffix = ","
	}
	return "(" + strings.Join(parts, ", ") + suffix + ")"
}
func (v *validator) choice(value any, choices []string, name string, optional bool) any {
	if value == nil && optional {
		return nil
	}
	s, ok := value.(string)
	if !ok || !slices.Contains(choices, s) {
		v.fail(fmt.Sprintf("%s is one of %s, not %s", name, tuple(choices), pyvalue.Repr(value)))
	}
	return value
}
func absent(value, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}
func object(value any) Object { m, _ := value.(map[string]any); return m }
func text(value any) string   { s, _ := value.(string); return s }
func list(value any) []any    { l, _ := evidence.List(value); return l }
func contains(value any, item any) bool {
	for _, v := range list(value) {
		if v == item {
			return true
		}
	}
	return false
}
func sortedKeys(m Object) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
func (v *validator) keys(value any, name string) []any {
	if value == nil {
		return []any{}
	}
	values, ok := evidence.List(value)
	if !ok {
		v.fail(name + " is a list")
		return []any{}
	}
	unique := map[string]bool{}
	for _, x := range values {
		k := v.key(x, name+"[]", false)
		if s, ok := k.(string); ok {
			unique[s] = true
		}
	}
	keys := make([]string, 0, len(unique))
	for k := range unique {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, k)
	}
	return out
}
func (v *validator) target(value any, name string) any {
	if value == nil {
		return nil
	}
	m := v.closed(value, []string{"team", "project"}, name)
	return Object{"team": v.text(m["team"], name+".team", false, 600), "project": v.project(m["project"], name+".project", false)}
}

// ReadRegistry validates products.read_registry's closed input shape.
func ReadRegistry(value any) (Object, error) {
	v := &validator{}
	r := v.object(value, "product-registry/1", strings.Fields("schema product workspace team familyLabel repositories surfaces triageProject testTarget"), "a registry record")
	product := v.product(r["product"], "product", false)
	if product == "unclassified" {
		v.fail("'unclassified' is the pending-classification bucket, not a product")
	}
	surfaces, ok := absent(r["surfaces"], Object{}).(map[string]any)
	if !ok {
		v.fail("surfaces is an object keyed by surface")
	}
	watched := Object{}
	for _, surface := range sortedKeys(surfaces) {
		v.choice(surface, Surfaces, "a surface", false)
		spec := v.closed(surfaces[surface], []string{"method", "active"}, "surfaces."+surface)
		watched[surface] = Object{"method": v.text(spec["method"], "surfaces."+surface+".method", false, 600), "active": v.flag(spec["active"], "surfaces."+surface+".active", nil)}
	}
	out := Object{"schema": "product-registry/1", "product": product, "workspace": v.key(r["workspace"], "workspace", false), "team": v.text(r["team"], "team", false, 600), "familyLabel": v.text(r["familyLabel"], "familyLabel", false, 600), "repositories": v.keys(r["repositories"], "repositories"), "surfaces": watched, "triageProject": v.project(r["triageProject"], "triageProject", true), "testTarget": v.target(r["testTarget"], "testTarget")}
	if target := object(out["testTarget"]); target != nil && out["triageProject"] == target["project"] {
		v.fail(fmt.Sprintf("the test target project %s is also the triage project; a simulated record and a real one would share one project's target", pyvalue.Repr(target["project"])))
	}
	return out, v.err
}

// Coverage distinguishes disconnected collection from a watched surface without incidents.
func Coverage(registry Object) Object {
	answer := Object{}
	surfaces := object(registry["surfaces"])
	for _, surface := range Surfaces {
		spec := object(surfaces[surface])
		switch {
		case spec != nil && spec["active"] == true:
			answer[surface] = Object{"state": "watched", "method": spec["method"]}
		case spec != nil:
			answer[surface] = Object{"state": "unobserved", "method": spec["method"], "reason": "declared and switched off"}
		default:
			answer[surface] = Object{"state": "unobserved", "method": nil, "reason": "no collection method is connected"}
		}
	}
	return answer
}

func (v *validator) followUps(value any) []any {
	if value == nil {
		return []any{}
	}
	values, ok := evidence.List(value)
	if !ok {
		v.fail("followUpOf is a list of {issue, checks}")
		return []any{}
	}
	answer := []any{}
	for _, entry := range values {
		m := v.closed(entry, []string{"issue", "checks"}, "followUpOf[]")
		checks, ok := evidence.List(m["checks"])
		if !ok || len(checks) == 0 {
			v.fail("followUpOf[].checks names at least one check")
		}
		issue := v.issue(m["issue"], "followUpOf[].issue", false)
		unique := map[string]bool{}
		for _, c := range checks {
			v.choice(c, Checks, "followUpOf[].checks[]", false)
			if s, ok := c.(string); ok {
				unique[s] = true
			}
		}
		names := make([]string, 0, len(unique))
		for s := range unique {
			names = append(names, s)
		}
		slices.Sort(names)
		answer = append(answer, Object{"issue": issue, "checks": names})
	}
	slices.SortStableFunc(answer, func(a, b any) int { return strings.Compare(text(object(a)["issue"]), text(object(b)["issue"])) })
	return answer
}

// ReadBinding validates one tracker snapshot, including test-target isolation.
func ReadBinding(value any, registry Object) (Object, error) {
	v := &validator{}
	r := v.object(value, "product-binding/1", strings.Fields("schema product kind ref title state project components symptoms goal fixRef followUpOf test observedAt source"), "a binding")
	kind := v.choice(r["kind"], []string{"project", "issue"}, "kind", false)
	test := v.flag(r["test"], "test", false)
	out := Object{"schema": "product-binding/1", "product": v.product(r["product"], "product", false), "kind": kind, "title": v.text(r["title"], "title", false, 600), "components": v.keys(r["components"], "components"), "test": test, "observedAt": v.text(r["observedAt"], "observedAt", true, 600), "source": v.text(r["source"], "source", false, 600)}
	if kind == "project" {
		for _, k := range []string{"project", "symptoms", "fixRef", "followUpOf"} {
			if r[k] != nil {
				v.fail("a project binding carries no " + k)
			}
		}
		out["ref"] = v.project(r["ref"], "ref", false)
		out["state"] = v.choice(r["state"], []string{"active", "completed"}, "state", false)
		out["goal"] = v.key(r["goal"], "goal", true)
	} else {
		if r["goal"] != nil {
			v.fail("an issue binding carries no goal; its project does")
		}
		out["ref"] = v.issue(r["ref"], "ref", false)
		out["state"] = v.choice(r["state"], []string{"open", "in_progress", "done", "canceled"}, "state", false)
		out["project"] = v.project(r["project"], "project", true)
		out["symptoms"] = v.keys(r["symptoms"], "symptoms")
		out["fixRef"] = v.text(r["fixRef"], "fixRef", true, 600)
		out["followUpOf"] = v.followUps(r["followUpOf"])
	}
	if registry != nil {
		if registry["product"] != out["product"] {
			v.fail(fmt.Sprintf("this binding names %s, not %s", out["product"], registry["product"]))
		}
		target := object(registry["testTarget"])
		where := out["project"]
		if kind == "project" {
			where = out["ref"]
		}
		if test == true {
			if target == nil {
				v.fail(fmt.Sprintf("%s has no test target, so nothing of it is a test binding", out["product"]))
			} else {
				if where != target["project"] {
					v.fail(fmt.Sprintf("a test binding sits on the test target project %s, not %s", pyvalue.Repr(target["project"]), pyvalue.Repr(where)))
				}
				if kind == "issue" && strings.Split(text(out["ref"]), "-")[0] != target["team"] {
					v.fail(fmt.Sprintf("a test issue belongs to the test target team %s", pyvalue.Repr(target["team"])))
				}
			}
		} else if target != nil && where == target["project"] {
			v.fail(fmt.Sprintf("%s is the test target project; only a test binding sits there, or a simulated record and a real one would share one project's target", pyvalue.Repr(where)))
		}
	}
	return out, v.err
}

// ReadPolicy requires an explicit basis and never allows project creation from counts alone.
func ReadPolicy(value any) (Object, error) {
	v := &validator{}
	r := v.object(value, "routing-policy/1", strings.Fields("schema policy enabled minIndependentFixes requireSharedGoal requireCompletionCriteria basis"), "a routing policy")
	v.choice(r["policy"], []string{"project_creation"}, "policy", false)
	n, ok := evidence.PyInt(r["minIndependentFixes"])
	if !ok || n < 2 {
		v.fail("minIndependentFixes is an integer of at least 2; one fix is not a project")
	}
	for _, k := range []string{"requireSharedGoal", "requireCompletionCriteria"} {
		if r[k] != true {
			v.fail(k + " is true: a project is never created from counts alone")
		}
	}
	return Object{"schema": "routing-policy/1", "policy": "project_creation", "enabled": v.flag(r["enabled"], "enabled", nil), "minIndependentFixes": n, "requireSharedGoal": true, "requireCompletionCriteria": true, "basis": v.text(r["basis"], "basis", false, 600)}, v.err
}

func (v *validator) scalar(value any, refusal string) {
	switch x := value.(type) {
	case nil, bool, int, int64:
		return
	case json.Number:
		if n, err := x.Float64(); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return
		}
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return
		}
	}
	v.fail(refusal)
}
func (v *validator) causeSignature(value any) Object {
	m, ok := value.(map[string]any)
	if !ok || len(m) == 0 || len(m) > 16 {
		v.fail("cause.signature is a non-empty object of at most 16 fields")
		return m
	}
	for _, k := range sortedKeys(m) {
		v.key(k, "cause.signature key", false)
		if s, ok := m[k].(string); ok {
			if utf8.RuneCountInString(s) > 256 {
				v.fail("cause.signature." + k + " is longer than 256 characters")
			}
		} else {
			v.scalar(m[k], "cause.signature."+k+" is a scalar; a signature names a failure domain")
		}
	}
	if len(pyjson.Dumps(m, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})) > 1024 {
		v.fail("cause.signature is larger than 1024 bytes")
	}
	return m
}
func (v *validator) evidence(value any) []any {
	values, ok := evidence.List(absent(value, []any{}))
	if !ok {
		v.fail("evidence is a list")
		return []any{}
	}
	if len(values) > 16 {
		v.fail(fmt.Sprintf("evidence has %d entries; at most 16", len(values)))
	}
	entries := []any{}
	for i, x := range values {
		name := fmt.Sprintf("evidence[%d]", i)
		m := v.closed(x, []string{"kind", "ref", "source", "observed"}, name)
		entry := Object{"kind": v.key(m["kind"], name+".kind", false), "ref": v.text(m["ref"], name+".ref", false, 256)}
		if m["source"] != nil {
			entry["source"] = v.text(m["source"], name+".source", false, 128)
		}
		if m["observed"] != nil {
			observed, ok := m["observed"].(map[string]any)
			if !ok || len(observed) > 16 {
				v.fail(name + ".observed is an object of at most 16 readings")
			}
			for _, k := range sortedKeys(observed) {
				v.key(k, name+".observed key", false)
				if s, ok := observed[k].(string); ok {
					v.text(s, name+".observed."+k, false, 256)
				} else {
					v.scalar(observed[k], name+".observed."+k+" is a scalar reading")
				}
			}
			entry["observed"] = observed
		}
		entries = append(entries, entry)
	}
	if len(pyjson.Dumps(entries, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})) > 4096 {
		v.fail("evidence is larger than 4096 bytes; it names where to look, not the thing looked at")
	}
	return entries
}

// ReadIncident rejects unbounded payloads and preserves cause identity verbatim.
func ReadIncident(value any) (Object, error) {
	v := &validator{}
	r := v.object(value, "product-incident/1", strings.Fields("schema product repository workspace surface phase component symptom severity expected origin occurrenceKey observedAt context cause goal detail evidence"), "an incident")
	ctx := v.closed(absent(r["context"], Object{}), strings.Fields("currentIssue run session regressionOf"), "context")
	var cause, goal any
	if r["cause"] != nil {
		c := v.closed(r["cause"], []string{"product", "faultId", "signature"}, "cause")
		m := Object{"product": v.product(c["product"], "cause.product", false), "faultId": v.text(c["faultId"], "cause.faultId", true, 600), "signature": nil}
		if c["signature"] != nil {
			m["signature"] = v.causeSignature(c["signature"])
		}
		cause = m
	}
	if r["goal"] != nil {
		g := v.closed(r["goal"], []string{"key", "criteria"}, "goal")
		goal = Object{"key": v.key(g["key"], "goal.key", false), "criteria": v.text(g["criteria"], "goal.criteria", true, 600)}
	}
	detailKeys := strings.Fields("impact expected actual reproduction owner nextAction")
	detail := v.closed(absent(r["detail"], Object{}), detailKeys, "detail")
	out := Object{"schema": "product-incident/1", "product": v.product(r["product"], "product", true), "repository": v.key(r["repository"], "repository", true), "workspace": v.key(r["workspace"], "workspace", true), "surface": v.choice(r["surface"], Surfaces, "surface", false), "phase": v.choice(r["phase"], []string{"development", "in_use"}, "phase", false), "component": v.key(r["component"], "component", false), "symptom": v.key(r["symptom"], "symptom", false), "severity": v.choice(r["severity"], []string{"notice", "degraded", "broken"}, "severity", false), "expected": v.choice(r["expected"], []string{"cancelled", "awaiting_approval", "unsupported"}, "expected", true), "origin": v.choice(absent(r["origin"], "observed"), []string{"observed", "simulated"}, "origin", false), "occurrenceKey": v.text(r["occurrenceKey"], "occurrenceKey", false, 256), "observedAt": v.text(r["observedAt"], "observedAt", true, 64)}
	out["context"] = Object{"currentIssue": v.issue(ctx["currentIssue"], "context.currentIssue", true), "run": v.text(ctx["run"], "context.run", true, 128), "session": v.text(ctx["session"], "context.session", true, 128), "regressionOf": v.text(ctx["regressionOf"], "context.regressionOf", true, 256)}
	out["cause"], out["goal"] = cause, goal
	d := Object{}
	for _, k := range detailKeys {
		d[k] = v.text(detail[k], "detail."+k, true, 600)
	}
	out["detail"] = d
	out["evidence"] = v.evidence(r["evidence"])
	return out, v.err
}

// Canonical is products.canonical: sorted UTF-8 JSON without whitespace or non-finite numbers.
func Canonical(value any) (string, error) {
	var bad string
	var finite func(any) bool
	finite = func(v any) bool {
		switch x := v.(type) {
		case float64:
			switch {
			case math.IsNaN(x):
				bad = "nan"
			case math.IsInf(x, 1):
				bad = "inf"
			case math.IsInf(x, -1):
				bad = "-inf"
			default:
				return true
			}
			return false
		case map[string]any:
			for _, key := range sortedKeys(x) {
				if !finite(x[key]) {
					return false
				}
			}
		case []any:
			for _, v := range x {
				if !finite(v) {
					return false
				}
			}
		}
		return true
	}
	if !finite(value) {
		return "", malformed("a value has no JSON text: Out of range float values are not JSON compliant: " + bad)
	}
	return pyjson.Dumps(value, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}), nil
}

// ReadClassification names who classified a pending incident; severity cannot be changed here.
func ReadClassification(value any) (Object, error) {
	v := &validator{}
	r := v.closed(value, strings.Fields("product component symptom goal by"), "a classification")
	var goal any
	if r["goal"] != nil {
		g := v.closed(r["goal"], []string{"key", "criteria"}, "classification.goal")
		goal = Object{"key": v.key(g["key"], "goal.key", false), "criteria": v.text(g["criteria"], "goal.criteria", true, 600)}
	}
	by := v.text(r["by"], "by", false, 128)
	if by != "operator" && !strings.HasPrefix(text(by), "llm:") {
		v.fail("by is operator or llm:<model>")
	}
	return Object{"product": v.product(r["product"], "product", false), "component": v.key(r["component"], "component", true), "symptom": v.key(r["symptom"], "symptom", true), "goal": goal, "by": by}, v.err
}

// ReadPage refuses nonsensical bounds instead of turning them into permission to write.
func ReadPage(limit, after any, ceiling int64) (int64, any, error) {
	n, ok := evidence.PyInt(limit)
	if !ok || n < 1 {
		return 0, nil, malformed("limit is a positive whole number, not " + pyvalue.Repr(limit))
	}
	if after != nil {
		cursor, ok := evidence.PyInt(after)
		if !ok || cursor < 0 {
			return 0, nil, malformed("after is a non-negative rowid cursor, not " + pyvalue.Repr(after))
		}
	}
	return min(n, ceiling), after, nil
}
