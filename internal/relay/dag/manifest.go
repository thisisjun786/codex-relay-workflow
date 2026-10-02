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
	// texts are non-empty strings; ints are whole numbers.
	texts []string
	ints  []string
}

// perKind lists the fields 4.2 requires of an input of each edge kind. The head of a code artifact is
// judged by the scheduler, which knows whether the source is code.
var perKind = map[string]manifestRule{
	EdgeArtifactVerified: {texts: []string{"acceptance_id", "relationship_id", "event_id", "revision_hash"}, ints: []string{"execution_generation"}},
	EdgeIntegrated:       {texts: []string{"acceptance_id", "head_sha", "landed_sha"}},
	EdgeDecision:         {texts: []string{"decision_id", "decision_digest"}, ints: []string{"decision_revision"}},
}

// CheckManifest reads a manifest body strictly: the required fields of 4.2 present and of their type (a missing
// value, null and the empty string are the same thing, 4.1), every input shaped for its edge kind, and the optional
// base, artifacts and volatile snapshots shaped as 4.2 shapes them. Whether a base is required depends on the node's
// kind (an implementation node has one, a non_pr node may have none), which the scheduler knows and a manifest does not carry.
func CheckManifest(body map[string]any) []Violation {
	var vs []Violation
	add := func(rule, path, format string, args ...any) {
		vs = append(vs, Violation{Rule: rule, Path: path, Detail: fmt.Sprintf(format, args...)})
	}
	text := func(obj map[string]any, path, key string) {
		if s, ok := obj[key].(string); !ok || s == "" {
			add(RuleMissingField, path+"."+key, "required text field is missing, empty or not text")
		}
	}
	digest := func(obj map[string]any, path, key string) {
		if s, ok := obj[key].(string); !ok || !digestPattern.MatchString(s) {
			add(RuleBadDigest, path+"."+key, "must be 64 lowercase hexadecimal characters")
		}
	}
	whole := func(obj map[string]any, path, key string, min int64) {
		n, ok := obj[key].(int64)
		switch {
		case obj[key] == nil:
			add(RuleMissingField, path+"."+key, "required whole number is missing")
		case !ok || n < min:
			add(RuleWrongType, path+"."+key, "must be a whole number of at least %d", min)
		}
	}
	for _, f := range []string{"node_id", "issue_key", "created_by_task_id", "created_at"} {
		text(body, "$", f)
	}
	if body["schema"] != SchemaManifest {
		add(RuleBadSchema, "$.schema", "must be %q", SchemaManifest)
	}
	digest(body, "$", "node_slice_digest")
	digest(body, "$", "criteria_set_digest")
	whole(body, "$", "plan_revision_no", 0)
	whole(body, "$", "coordinator_epoch", 0)
	if rv, ok := body["rule_version"].(map[string]any); !ok || len(rv) == 0 {
		add(RuleMissingField, "$.rule_version", "required object is missing or empty")
	}
	// 4.1: a missing value and a null are the same thing, so a null optional field is an absent one.
	if base := body["base"]; base != nil {
		obj, ok := base.(map[string]any)
		if !ok {
			add(RuleNotAnObject, "$.base", "must be a JSON object with repository, ref and sha")
		} else {
			for _, f := range []string{"repository", "ref", "sha"} {
				text(obj, "$.base", f)
			}
		}
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
		for _, f := range append([]string{"edge_id", "from_node_id"}, rule.texts...) {
			text(in, path, f)
		}
		for _, f := range rule.ints {
			whole(in, path, f, 0)
		}
		if artifacts := in["artifacts"]; artifacts != nil {
			list, ok := artifacts.([]any)
			if !ok {
				add(RuleWrongType, path+".artifacts", "must be a list")
			}
			for j, a := range list {
				ap := fmt.Sprintf("%s.artifacts[%d]", path, j)
				obj, ok := a.(map[string]any)
				if !ok {
					add(RuleNotAnObject, ap, "must be a JSON object with uri, sha256, bytes and scope")
					continue
				}
				text(obj, ap, "uri")
				text(obj, ap, "scope")
				digest(obj, ap, "sha256")
				whole(obj, ap, "bytes", 0)
			}
		}
	}
	if volatile := body["volatile"]; volatile != nil {
		list, ok := volatile.([]any)
		if !ok {
			add(RuleWrongType, "$.volatile", "must be a list")
		}
		for i, v := range list {
			vp := fmt.Sprintf("$.volatile[%d]", i)
			obj, ok := v.(map[string]any)
			if !ok {
				add(RuleNotAnObject, vp, "must be a JSON object with source, snapshot_uri, sha256 and captured_at")
				continue
			}
			text(obj, vp, "source")
			text(obj, vp, "snapshot_uri")
			text(obj, vp, "captured_at")
			digest(obj, vp, "sha256")
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

// dropNil is a copy of m without the keys whose value is null: for the digest a null is a missing value (4.1).
func dropNil(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// ManifestDigest is the digest of a manifest body: canonical JSON of the fields 4.2 marks as included
// (rule version, the plan revision, the epoch, the author and the times are recorded but left out, so
// they never change what the manifest is), with inputs ordered by edge id and artifacts by uri. A null
// value is a missing value (4.1), so a manifest that spells an absent field null digests like one that omits it.
func ManifestDigest(body map[string]any) string {
	inputs, _ := body["inputs"].([]any)
	ordered := make([]any, 0, len(inputs))
	for _, item := range sortedBy(inputs, "edge_id") {
		in, ok := item.(map[string]any)
		if !ok {
			ordered = append(ordered, item)
			continue
		}
		copied := dropNil(in)
		if artifacts, ok := copied["artifacts"].([]any); ok {
			kept := make([]any, 0, len(artifacts))
			for _, a := range sortedBy(artifacts, "uri") {
				if m, ok := a.(map[string]any); ok {
					a = dropNil(m)
				}
				kept = append(kept, a)
			}
			copied["artifacts"] = kept
		}
		ordered = append(ordered, copied)
	}
	included := map[string]any{"inputs": ordered}
	for _, f := range []string{"schema", "node_id", "issue_key", "node_slice_digest", "criteria_set_digest"} {
		if v := body[f]; v != nil {
			included[f] = v
		}
	}
	if base, ok := body["base"].(map[string]any); ok {
		included["base"] = dropNil(base)
	}
	if volatile, ok := body["volatile"].([]any); ok {
		kept := make([]any, 0, len(volatile))
		for _, item := range sortedBy(volatile, "source", "snapshot_uri") {
			if m, ok := item.(map[string]any); ok {
				copied := dropNil(m)
				delete(copied, "captured_at")
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
	var body map[string]any
	var found bool
	err := r.readTx(ctx, func(q Queryer) (err error) {
		body, found, err = ReadManifestOn(ctx, q, digest)
		return err
	})
	return body, found, err
}

// ReadManifestOn is ReadManifest on a reader the caller already holds: a transaction's connection, where
// opening another would wait for the store's one connection (the release path reads a manifest inside the
// transaction that writes its intent). It needs only QueryContext, so a store.Querier is accepted as is.
func ReadManifestOn(ctx context.Context, q Queryer, digest string) (map[string]any, bool, error) {
	var stored string
	found, err := scanOne(ctx, q, "SELECT body_json FROM dag_input_manifests WHERE manifest_digest = ?", []any{digest}, &stored)
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
