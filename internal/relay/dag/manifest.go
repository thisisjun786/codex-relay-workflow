package dag

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The input manifest (contract 4.2) is the record of what a node consumed: its slice, its criteria,
// its base, and for every incoming edge the value that satisfied it. It is content-addressed: the
// digest covers the fields 4.2 marks as included and has no plan id, so one node consuming the same
// things twice is one record. This file is the schema side only (digest, strict shape, storage);
// building a manifest from a plan's state and judging it fit to release is the scheduler's.

type manifestRule struct {
	required []string
}

// perKind lists the fields 4.2 requires of an input of each edge kind. The head of a code artifact is
// judged by the scheduler, which knows whether the source is code.
var perKind = map[string]manifestRule{
	EdgeArtifactVerified: {required: []string{"acceptance_id", "relationship_id", "execution_generation", "event_id", "revision_hash"}},
	EdgeIntegrated:       {required: []string{"acceptance_id", "head_sha", "landed_sha"}},
	EdgeDecision:         {required: []string{"decision_id", "decision_digest", "decision_revision"}},
}

func missing(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	}
	return false
}

// CheckManifest reads a manifest body strictly: the required fields of 4.2 present and non-empty (a missing
// value, null and the empty string are the same thing, 4.1) and every input shaped for its edge kind.
func CheckManifest(body map[string]any) []Violation {
	var vs []Violation
	add := func(rule, path, format string, args ...any) {
		vs = append(vs, Violation{Rule: rule, Path: path, Detail: fmt.Sprintf(format, args...)})
	}
	for _, f := range []string{"node_id", "issue_key", "created_by_task_id", "created_at"} {
		if s, ok := body[f].(string); !ok || s == "" {
			add(RuleMissingField, "$."+f, "required text field is missing or empty")
		}
	}
	if body["schema"] != SchemaManifest {
		add(RuleBadSchema, "$.schema", "must be %q", SchemaManifest)
	}
	for _, f := range []string{"node_slice_digest", "criteria_set_digest"} {
		if s, ok := body[f].(string); !ok || !digestPattern.MatchString(s) {
			add(RuleBadDigest, "$."+f, "must be 64 lowercase hexadecimal characters")
		}
	}
	for _, f := range []string{"plan_revision_no", "coordinator_epoch"} {
		if _, ok := body[f].(int64); !ok {
			add(RuleWrongType, "$."+f, "must be a whole number")
		}
	}
	if rv, ok := body["rule_version"].(map[string]any); !ok || len(rv) == 0 {
		add(RuleMissingField, "$.rule_version", "required object is missing or empty")
	}
	inputs, ok := body["inputs"].([]any)
	if !ok {
		add(RuleMissingField, "$.inputs", "required list is missing (an empty list is allowed: a node with no incoming edge)")
	}
	for i, item := range inputs {
		path := fmt.Sprintf("$.inputs[%d]", i)
		in, ok := item.(map[string]any)
		if !ok {
			add(RuleNotAnObject, path, "must be a JSON object")
			continue
		}
		kind, _ := in["kind"].(string)
		rule, known := perKind[kind]
		if !known {
			add(RuleUnknownEdgeKind, path+".kind", "%q is not one of %s, %s, %s", kind, EdgeArtifactVerified, EdgeIntegrated, EdgeDecision)
			continue
		}
		for _, f := range append([]string{"edge_id", "from_node_id"}, rule.required...) {
			if missing(in[f]) {
				add(RuleMissingField, path+"."+f, "required for an %s input and missing or empty", kind)
			}
		}
	}
	return vs
}

// sortedBy copies a list of objects ordered by the string fields named, so arrays digest in the order 4.1 fixes.
func sortedBy(list []any, fields ...string) []any {
	out := append([]any(nil), list...)
	key := func(v any) string {
		m, _ := v.(map[string]any)
		k := ""
		for _, f := range fields {
			s, _ := m[f].(string)
			k += s + "\x00"
		}
		return k
	}
	sort.SliceStable(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

// ManifestDigest is the digest of a manifest body: canonical JSON of the fields 4.2 marks as included
// (rule version, the plan revision, the epoch, the author and the times are recorded but left out, so
// they never change what the manifest is), with inputs ordered by edge id and artifacts by uri.
func ManifestDigest(body map[string]any) string {
	inputs, _ := body["inputs"].([]any)
	ordered := make([]any, 0, len(inputs))
	for _, item := range sortedBy(inputs, "edge_id") {
		in, ok := item.(map[string]any)
		if !ok {
			ordered = append(ordered, item)
			continue
		}
		copied := map[string]any{}
		for k, v := range in {
			copied[k] = v
		}
		if artifacts, ok := in["artifacts"].([]any); ok {
			copied["artifacts"] = sortedBy(artifacts, "uri")
		}
		ordered = append(ordered, copied)
	}
	included := map[string]any{"inputs": ordered}
	for _, f := range []string{"schema", "node_id", "issue_key", "node_slice_digest", "criteria_set_digest", "base"} {
		if v, ok := body[f]; ok {
			included[f] = v
		}
	}
	if volatile, ok := body["volatile"].([]any); ok {
		kept := make([]any, 0, len(volatile))
		for _, item := range sortedBy(volatile, "source", "snapshot_uri") {
			if m, ok := item.(map[string]any); ok {
				copied := map[string]any{}
				for k, v := range m {
					if k != "captured_at" {
						copied[k] = v
					}
				}
				kept = append(kept, copied)
			}
		}
		included["volatile"] = kept
	}
	return sum(included)
}

// PutManifest stores a manifest body (it must read strictly, and a manifest_digest it carries must be the
// one recomputed) and returns its digest. A digest that is already stored is the same manifest, so the
// stored row stays and the call succeeds (idempotent).
func (r *Repo) PutManifest(ctx context.Context, raw []byte) (string, error) {
	value, err := parse(string(raw))
	if err != nil {
		return "", &UnreadableError{Detail: err.Error()}
	}
	body, ok := value.(map[string]any)
	if !ok {
		return "", &PlanRejected{Violations: []Violation{{Rule: RuleNotAnObject, Path: "$", Detail: "a manifest is a JSON object"}}}
	}
	if vs := CheckManifest(body); len(vs) > 0 {
		return "", &PlanRejected{Violations: vs}
	}
	digest := ManifestDigest(body)
	if claimed, present := body["manifest_digest"]; present && claimed != digest {
		return "", &store.RefusedError{Reason: string(contract.RefusalRevisionMismatch), Detail: fmt.Sprintf("the manifest carries digest %v but its content digests to %s", claimed, digest)}
	}
	body["manifest_digest"] = digest
	err = r.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			digest, body["node_id"], canonical(body), canonical(body["rule_version"]), body["coordinator_epoch"], body["created_at"])
		return err
	})
	return digest, err
}

// ReadManifest returns a stored manifest body and verifies it: the digest of the body read must be the
// key it is stored under.
func (r *Repo) ReadManifest(ctx context.Context, digest string) (map[string]any, bool, error) {
	var stored string
	var found bool
	err := r.readTx(ctx, func(q Queryer) (err error) {
		found, err = scanOne(ctx, q, "SELECT body_json FROM dag_input_manifests WHERE manifest_digest = ?", []any{digest}, &stored)
		return err
	})
	if err != nil || !found {
		return nil, false, err
	}
	value, err := parse(stored)
	body, ok := value.(map[string]any)
	if err != nil || !ok {
		return nil, true, &CorruptError{Detail: "the stored manifest " + digest + " is not a JSON object"}
	}
	if got := ManifestDigest(body); got != digest {
		return nil, true, &CorruptError{Detail: fmt.Sprintf("the stored manifest %s digests to %s", digest, got)}
	}
	return body, true, nil
}
