package dag

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	digestPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// UnreadableError is a document that is not JSON at all: nothing about a plan can be said of it.
type UnreadableError struct{ Detail string }

func (e *UnreadableError) Error() string {
	return "the revision document is not valid JSON: " + e.Detail
}

// hints say what to do instead when a field is a plausible mistake. A field this reader does not
// know is rejected, never skipped: a plan whose nodes were silently dropped would be an empty plan.
var hints = map[string]string{
	"nodes": "a revision carries its nodes and edges as changes: {\"op\":\"add_node\",\"node\":{...}} entries in changes[], and a top-level \"nodes\" is not read",
	"edges": "a revision carries its nodes and edges as changes: {\"op\":\"add_edge\",\"edge\":{...}} entries in changes[], and a top-level \"edges\" is not read",
}

var (
	nodeKinds = []string{NodeImplementation, NodeNonPR}
	edgeKinds = []string{EdgeArtifactVerified, EdgeIntegrated, EdgeDecision}
	allOps    = []string{OpAddNode, OpUpdateNode, OpReplaceNode, OpRetireNode, OpAddEdge, OpRetireEdge}
)

type decoder struct{ violations []Violation }

func (d *decoder) add(rule, path, format string, args ...any) {
	d.violations = append(d.violations, Violation{Rule: rule, Path: path, Detail: fmt.Sprintf(format, args...)})
}

// object reads value as an object with exactly the required and optional keys.
func (d *decoder) object(path string, value any, required, optional []string) (map[string]any, bool) {
	obj, ok := value.(map[string]any)
	if !ok {
		d.add(RuleNotAnObject, path, "must be a JSON object")
		return nil, false
	}
	known := map[string]bool{}
	for _, key := range required {
		known[key] = true
	}
	for _, key := range optional {
		known[key] = true
	}
	var unknown []string
	for key := range obj {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		detail := fmt.Sprintf("unknown field %q", key)
		if hint, ok := hints[key]; ok && path == "$" {
			detail += ": " + hint
		}
		d.add(RuleUnknownField, join(path, key), "%s", detail)
	}
	ok = len(unknown) == 0
	for _, key := range required {
		if _, present := obj[key]; !present {
			d.add(RuleMissingField, join(path, key), "required field is missing")
			ok = false
		}
	}
	return obj, ok
}

func join(path, key string) string { return path + "." + key }

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// text reads a required or optional string field: non-empty when present, bounded, no control characters.
func (d *decoder) text(path string, obj map[string]any, key string, max int, required bool) (string, bool) {
	value, present := obj[key]
	if !present {
		return "", false
	}
	p := join(path, key)
	s, ok := value.(string)
	switch {
	case !ok:
		d.add(RuleWrongType, p, "must be a string")
	case s == "":
		d.add(RuleEmptyValue, p, "must not be empty (an empty string is a missing value)")
	case utf8.RuneCountInString(s) > max:
		d.add(RuleValueTooLong, p, "is %d characters long; the limit is %d", utf8.RuneCountInString(s), max)
	case hasControl(s):
		d.add(RuleBadText, p, "must not contain control characters")
	default:
		return s, true
	}
	return "", false
}

// identifier reads a field that names something: letters, digits, '.', '_', ':' and '-', at most MaxIDLength.
func (d *decoder) identifier(path string, obj map[string]any, key string) (string, bool) {
	value, present := obj[key]
	if !present {
		return "", false
	}
	p := join(path, key)
	s, ok := value.(string)
	switch {
	case !ok:
		d.add(RuleWrongType, p, "must be a string")
	case s == "":
		d.add(RuleEmptyValue, p, "must not be empty (an empty string is a missing value)")
	case len(s) > MaxIDLength:
		d.add(RuleValueTooLong, p, "is %d characters long; the limit is %d", len(s), MaxIDLength)
	case !identifierPattern.MatchString(s):
		d.add(RuleBadIdentifier, p, "%q must start with a letter or digit and hold only letters, digits, '.', '_', ':' and '-'", s)
	default:
		return s, true
	}
	return "", false
}

func (d *decoder) digest(path string, obj map[string]any, key string) (string, bool) {
	value, present := obj[key]
	if !present {
		return "", false
	}
	p := join(path, key)
	s, ok := value.(string)
	switch {
	case !ok:
		d.add(RuleWrongType, p, "must be a string")
	case !digestPattern.MatchString(s):
		d.add(RuleBadDigest, p, "must be 64 lowercase hexadecimal characters (a sha256 digest)")
	default:
		return s, true
	}
	return "", false
}

// enum reads a string field that must be one of allowed.
func (d *decoder) enum(path string, obj map[string]any, key, rule string, allowed []string) (string, bool) {
	value, present := obj[key]
	if !present {
		return "", false
	}
	p := join(path, key)
	s, ok := value.(string)
	if !ok {
		d.add(RuleWrongType, p, "must be a string")
		return "", false
	}
	for _, a := range allowed {
		if s == a {
			return s, true
		}
	}
	d.add(rule, p, "%q is not one of %s", s, strings.Join(allowed, ", "))
	return "", false
}

// integer reads a non-negative whole number: a JSON integer, never a float or a boolean.
func (d *decoder) integer(path string, obj map[string]any, key string) (int64, bool) {
	value, present := obj[key]
	if !present {
		return 0, false
	}
	p := join(path, key)
	n, ok := value.(int64)
	switch {
	case !ok:
		d.add(RuleWrongType, p, "must be a whole number")
	case n < 0:
		d.add(RuleWrongType, p, "must not be negative")
	default:
		return n, true
	}
	return 0, false
}

func (d *decoder) boolean(path string, obj map[string]any, key string) (bool, bool) {
	value, present := obj[key]
	if !present {
		return false, false
	}
	b, ok := value.(bool)
	if !ok {
		d.add(RuleWrongType, join(path, key), "must be true or false")
		return false, false
	}
	return b, true
}

func (d *decoder) authorities(path string, obj map[string]any, key string) []string {
	value, present := obj[key]
	if !present {
		return nil
	}
	p := join(path, key)
	list, ok := value.([]any)
	if !ok {
		d.add(RuleWrongType, p, "must be a list of strings")
		return nil
	}
	if len(list) == 0 {
		d.add(RuleEmptyValue, p, "must name at least one authority")
		return nil
	}
	if len(list) > MaxAuthorities {
		d.add(RuleLimitExceeded, p, "names %d authorities; the limit is %d", len(list), MaxAuthorities)
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for i, item := range list {
		s, ok := item.(string)
		ip := fmt.Sprintf("%s[%d]", p, i)
		switch {
		case !ok:
			d.add(RuleWrongType, ip, "must be a string")
		case s == "":
			d.add(RuleEmptyValue, ip, "must not be empty")
		case utf8.RuneCountInString(s) > MaxAuthorityLength:
			d.add(RuleValueTooLong, ip, "is %d characters long; the limit is %d", utf8.RuneCountInString(s), MaxAuthorityLength)
		case hasControl(s):
			d.add(RuleBadText, ip, "must not contain control characters")
		case seen[s]:
			d.add(RuleDuplicateValue, ip, "%q is named twice", s)
		default:
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (d *decoder) node(path string, value any) *Node {
	obj, _ := d.object(path, value, []string{"node_id", "issue_key", "kind", "criteria_set_digest"}, []string{"title"})
	if obj == nil {
		return nil
	}
	n := &Node{}
	n.NodeID, _ = d.identifier(path, obj, "node_id")
	n.IssueKey, _ = d.identifier(path, obj, "issue_key")
	n.Kind, _ = d.enum(path, obj, "kind", RuleUnknownNodeKind, nodeKinds)
	n.CriteriaSetDigest, _ = d.digest(path, obj, "criteria_set_digest")
	n.Title, _ = d.text(path, obj, "title", MaxTitleLength, false)
	return n
}

func (d *decoder) edge(path string, value any) *Edge {
	obj, _ := d.object(path, value, []string{"edge_id", "from_node_id", "to_node_id", "kind"},
		[]string{"target_repository", "target_base_ref", "pins_code_head", "decision_subject", "decision_digest", "required_authority"})
	if obj == nil {
		return nil
	}
	e := &Edge{}
	e.EdgeID, _ = d.identifier(path, obj, "edge_id")
	e.FromNodeID, _ = d.identifier(path, obj, "from_node_id")
	e.ToNodeID, _ = d.identifier(path, obj, "to_node_id")
	e.Kind, _ = d.enum(path, obj, "kind", RuleUnknownEdgeKind, edgeKinds)
	e.TargetRepository, _ = d.text(path, obj, "target_repository", MaxTextLength, false)
	e.TargetBaseRef, _ = d.text(path, obj, "target_base_ref", MaxTextLength, false)
	e.PinsCodeHead, _ = d.boolean(path, obj, "pins_code_head")
	e.DecisionSubject, _ = d.text(path, obj, "decision_subject", MaxTextLength, false)
	e.DecisionDigest, _ = d.digest(path, obj, "decision_digest")
	e.RequiredAuthority = d.authorities(path, obj, "required_authority")
	return e
}

func (d *decoder) change(path string, value any) (Change, bool) {
	raw, isObject := value.(map[string]any)
	if !isObject {
		d.add(RuleNotAnObject, path, "must be a JSON object")
		return Change{}, false
	}
	op, ok := d.enum(path, raw, "op", RuleUnknownOp, allOps)
	if !ok {
		if _, present := raw["op"]; !present {
			d.add(RuleMissingField, join(path, "op"), "required field is missing")
		}
		return Change{}, false
	}
	c := Change{Op: op}
	switch op {
	case OpAddNode, OpUpdateNode:
		obj, _ := d.object(path, value, []string{"op", "node"}, nil)
		if obj != nil {
			c.Node = d.node(join(path, "node"), obj["node"])
		}
	case OpReplaceNode:
		obj, _ := d.object(path, value, []string{"op", "node", "supersedes_node_id"}, nil)
		if obj != nil {
			c.Node = d.node(join(path, "node"), obj["node"])
			c.SupersedesNodeID, _ = d.identifier(path, obj, "supersedes_node_id")
		}
	case OpRetireNode:
		obj, _ := d.object(path, value, []string{"op", "node_id"}, nil)
		if obj != nil {
			c.NodeID, _ = d.identifier(path, obj, "node_id")
		}
	case OpAddEdge:
		obj, _ := d.object(path, value, []string{"op", "edge"}, nil)
		if obj != nil {
			c.Edge = d.edge(join(path, "edge"), obj["edge"])
		}
	case OpRetireEdge:
		obj, _ := d.object(path, value, []string{"op", "edge_id"}, nil)
		if obj != nil {
			c.EdgeID, _ = d.identifier(path, obj, "edge_id")
		}
	}
	return c, true
}

func (d *decoder) changes(path string, value any) []Change {
	list, ok := value.([]any)
	if !ok {
		d.add(RuleWrongType, path, "must be a list of changes")
		return nil
	}
	if len(list) == 0 {
		d.add(RuleEmptyChanges, path, "a revision must carry at least one change")
		return nil
	}
	if len(list) > MaxChanges {
		d.add(RuleLimitExceeded, path, "carries %d changes; the limit is %d", len(list), MaxChanges)
		return nil
	}
	out := make([]Change, 0, len(list))
	for i, item := range list {
		if c, ok := d.change(fmt.Sprintf("%s[%d]", path, i), item); ok {
			out = append(out, c)
		}
	}
	return out
}

func parse(raw string) (any, error) {
	return pyjson.Loads(raw, pyjson.LoadOptions{Python: true, Map: true, Unique: true, Numbers: pyjson.Int64Numbers})
}

// DecodeRevision reads a revision document strictly and returns it normalised. A document that is not
// JSON is an *UnreadableError; one that is JSON and breaks a rule of the document is a *PlanRejected
// naming every rule it breaks (and nothing was decided about the plan it would produce).
func DecodeRevision(raw []byte) (Revision, error) {
	if len(raw) > MaxDocumentBytes {
		return Revision{}, &PlanRejected{Violations: []Violation{{Rule: RuleLimitExceeded, Path: "$",
			Detail: fmt.Sprintf("the document is %d bytes; the limit is %d", len(raw), MaxDocumentBytes)}}}
	}
	value, err := parse(string(raw))
	if err != nil {
		var repeated *pyjson.RepeatedKey
		if errors.As(err, &repeated) {
			return Revision{}, &PlanRejected{Violations: []Violation{{Rule: RuleDuplicateKey, Path: "$", Detail: repeated.Error()}}}
		}
		return Revision{}, &UnreadableError{Detail: err.Error()}
	}
	d := &decoder{}
	obj, _ := d.object("$", value, []string{"schema", "plan_id", "project_key", "request_id", "expected_parent_revision", "author_task_id", "changes"}, []string{"coordinator_epoch"})
	var rev Revision
	if obj != nil {
		if schema, ok := obj["schema"].(string); !ok || schema != SchemaRevision {
			if _, present := obj["schema"]; present {
				d.add(RuleBadSchema, "$.schema", "must be %q", SchemaRevision)
			}
		}
		rev.PlanID, _ = d.identifier("$", obj, "plan_id")
		rev.ProjectKey, _ = d.identifier("$", obj, "project_key")
		rev.RequestID, _ = d.identifier("$", obj, "request_id")
		rev.AuthorTaskID, _ = d.identifier("$", obj, "author_task_id")
		rev.ExpectedParent, _ = d.integer("$", obj, "expected_parent_revision")
		rev.CoordinatorEpoch, _ = d.integer("$", obj, "coordinator_epoch")
		if changes, present := obj["changes"]; present {
			rev.Changes = d.changes("$.changes", changes)
		}
	}
	if len(d.violations) > 0 {
		return Revision{}, &PlanRejected{Violations: d.violations}
	}
	return rev, nil
}

// DecodeChanges reads the change list a stored revision holds (the same strict reader). A stored
// list that does not read is corruption, not a rejected request.
func DecodeChanges(stored string) ([]Change, error) {
	value, err := parse(stored)
	if err != nil {
		return nil, &CorruptError{Detail: "a stored change list is not JSON: " + err.Error()}
	}
	d := &decoder{}
	changes := d.changes("changes", value)
	if len(d.violations) > 0 {
		return nil, &CorruptError{Detail: "a stored change list does not read: " + (&PlanRejected{Violations: d.violations}).refusal().Detail}
	}
	return changes, nil
}

// Checked is the revision a typed request is, judged by the rules of a revision document: it
// writes the request as the document it would have been and reads that back with DecodeRevision, so
// a Go caller of Put cannot store what the command line would have refused (an empty change list,
// an op that does not exist, a malformed digest or identifier, a change without its node or edge).
// What it returns is the normalised revision Put goes on with.
func Checked(rev Revision) (Revision, error) {
	var shape []Violation
	for i, c := range rev.Changes {
		missing := ""
		switch {
		case (c.Op == OpAddNode || c.Op == OpUpdateNode || c.Op == OpReplaceNode) && c.Node == nil:
			missing = "node"
		case c.Op == OpAddEdge && c.Edge == nil:
			missing = "edge"
		}
		if missing != "" {
			shape = append(shape, Violation{Rule: RuleMissingField, Path: fmt.Sprintf("$.changes[%d].%s", i, missing),
				Detail: fmt.Sprintf("%s needs its %s", c.Op, missing)})
		}
	}
	if len(shape) > 0 {
		return Revision{}, &PlanRejected{Violations: shape}
	}
	changes := make([]any, len(rev.Changes))
	for i, c := range rev.Changes {
		changes[i] = changeObject(c)
	}
	return DecodeRevision([]byte(canonical(map[string]any{
		"schema": SchemaRevision, "plan_id": rev.PlanID, "project_key": rev.ProjectKey, "request_id": rev.RequestID,
		"expected_parent_revision": rev.ExpectedParent, "coordinator_epoch": rev.CoordinatorEpoch, "author_task_id": rev.AuthorTaskID,
		"changes": changes,
	})))
}
