package dag

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func manifestBody() doc {
	return doc{
		"schema": SchemaManifest, "node_id": "impl-a", "issue_key": "CRW-impl-a",
		"node_slice_digest": dig("slice"), "criteria_set_digest": dig("criteria impl-a"),
		"base": doc{"repository": "owner/repo", "ref": "dev", "sha": "abc123"},
		"inputs": []any{
			doc{"edge_id": "e2", "kind": EdgeIntegrated, "from_node_id": "impl-b", "acceptance_id": "acc-2", "head_sha": "h2", "landed_sha": "l2"},
			doc{"edge_id": "e1", "kind": EdgeArtifactVerified, "from_node_id": "design", "acceptance_id": "acc-1", "relationship_id": "rel-1",
				"execution_generation": 1, "event_id": "evt", "revision_hash": dig("rev"),
				"artifacts": []any{doc{"uri": "/art/b", "sha256": dig("b"), "bytes": 2, "scope": "/art"}, doc{"uri": "/art/a", "sha256": dig("a"), "bytes": 1, "scope": "/art"}}},
		},
		"volatile":         []any{doc{"source": "linear", "snapshot_uri": "/snap/1", "sha256": dig("snap"), "captured_at": "2026-10-02T00:00:00Z"}},
		"rule_version":     doc{"model": "m", "effort": "high"},
		"plan_revision_no": 4, "coordinator_epoch": 2, "created_by_task_id": "task-a", "created_at": "2026-10-02T00:00:00Z",
	}
}

func parseBody(t *testing.T, d doc) map[string]any {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	v, err := parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func digestOf(t *testing.T, mutate func(doc)) string {
	t.Helper()
	d := manifestBody()
	if mutate != nil {
		mutate(d)
	}
	return ManifestDigest(parseBody(t, d))
}

// The digest covers what 4.2 marks as included and nothing else, whatever order the lists come in.
func TestManifestDigestCoversExactlyTheIncludedFields(t *testing.T) {
	base := digestOf(t, nil)
	for name, mutate := range map[string]func(doc){
		"rule_version":       func(d doc) { d["rule_version"] = doc{"model": "other"} },
		"plan_revision_no":   func(d doc) { d["plan_revision_no"] = 99 },
		"coordinator_epoch":  func(d doc) { d["coordinator_epoch"] = 99 },
		"created_by_task_id": func(d doc) { d["created_by_task_id"] = "someone-else" },
		"created_at":         func(d doc) { d["created_at"] = "2030-01-01T00:00:00Z" },
		"captured_at":        func(d doc) { d["volatile"].([]any)[0].(doc)["captured_at"] = "2030-01-01T00:00:00Z" },
		"manifest_digest":    func(d doc) { d["manifest_digest"] = "ignored" },
		"input order": func(d doc) {
			in := d["inputs"].([]any)
			in[0], in[1] = in[1], in[0]
		},
		"artifact order": func(d doc) {
			a := d["inputs"].([]any)[1].(doc)["artifacts"].([]any)
			a[0], a[1] = a[1], a[0]
		},
	} {
		if got := digestOf(t, mutate); got != base {
			t.Errorf("%s moved the digest", name)
		}
	}
	for name, mutate := range map[string]func(doc){
		"node_id":               func(d doc) { d["node_id"] = "impl-z" },
		"issue_key":             func(d doc) { d["issue_key"] = "CRW-9" },
		"node_slice_digest":     func(d doc) { d["node_slice_digest"] = dig("other slice") },
		"criteria_set_digest":   func(d doc) { d["criteria_set_digest"] = dig("other criteria") },
		"base.sha":              func(d doc) { d["base"].(doc)["sha"] = "def456" },
		"an input's acceptance": func(d doc) { d["inputs"].([]any)[0].(doc)["acceptance_id"] = "acc-9" },
		"an artifact's uri":     func(d doc) { d["inputs"].([]any)[1].(doc)["artifacts"].([]any)[0].(doc)["uri"] = "/art/z" },
		"an artifact's scope":   func(d doc) { d["inputs"].([]any)[1].(doc)["artifacts"].([]any)[0].(doc)["scope"] = "/other" },
		"a volatile snapshot":   func(d doc) { d["volatile"].([]any)[0].(doc)["sha256"] = dig("changed") },
		"an input removed":      func(d doc) { d["inputs"] = d["inputs"].([]any)[:1] },
	} {
		if got := digestOf(t, mutate); got == base {
			t.Errorf("%s did not move the digest", name)
		}
	}
}

func TestManifestShapeIsStrict(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(doc)
		rule   string
		path   string
	}{
		"missing rule_version":  {func(d doc) { delete(d, "rule_version") }, RuleMissingField, "$.rule_version"},
		"empty rule_version":    {func(d doc) { d["rule_version"] = doc{} }, RuleMissingField, "$.rule_version"},
		"null node_id":          {func(d doc) { d["node_id"] = nil }, RuleMissingField, "$.node_id"},
		"empty issue_key":       {func(d doc) { d["issue_key"] = "" }, RuleMissingField, "$.issue_key"},
		"bad slice digest":      {func(d doc) { d["node_slice_digest"] = "x" }, RuleBadDigest, "$.node_slice_digest"},
		"missing inputs":        {func(d doc) { delete(d, "inputs") }, RuleMissingField, "$.inputs"},
		"unknown input kind":    {func(d doc) { d["inputs"].([]any)[0].(doc)["kind"] = "blocks" }, RuleUnknownEdgeKind, "$.inputs[0].kind"},
		"integrated needs head": {func(d doc) { delete(d["inputs"].([]any)[0].(doc), "head_sha") }, RuleMissingField, "$.inputs[0].head_sha"},
		"artifact needs event":  {func(d doc) { d["inputs"].([]any)[1].(doc)["event_id"] = "" }, RuleMissingField, "$.inputs[1].event_id"},
		"generation as text":    {func(d doc) { d["inputs"].([]any)[1].(doc)["execution_generation"] = "one" }, RuleWrongType, "$.inputs[1].execution_generation"},
		"acceptance as number":  {func(d doc) { d["inputs"].([]any)[0].(doc)["acceptance_id"] = 7 }, RuleMissingField, "$.inputs[0].acceptance_id"},
		"base is a list":        {func(d doc) { d["base"] = []any{} }, RuleNotAnObject, "$.base"},
		"base without a sha":    {func(d doc) { delete(d["base"].(doc), "sha") }, RuleMissingField, "$.base.sha"},
		"artifact not a digest": {func(d doc) { d["inputs"].([]any)[1].(doc)["artifacts"].([]any)[0].(doc)["sha256"] = "x" }, RuleBadDigest, "$.inputs[1].artifacts[0].sha256"},
		"artifact bytes text":   {func(d doc) { d["inputs"].([]any)[1].(doc)["artifacts"].([]any)[0].(doc)["bytes"] = "two" }, RuleWrongType, "$.inputs[1].artifacts[0].bytes"},
		"artifacts not a list":  {func(d doc) { d["inputs"].([]any)[1].(doc)["artifacts"] = "x" }, RuleWrongType, "$.inputs[1].artifacts"},
		"volatile without uri":  {func(d doc) { delete(d["volatile"].([]any)[0].(doc), "snapshot_uri") }, RuleMissingField, "$.volatile[0].snapshot_uri"},
		"revision as text":      {func(d doc) { d["plan_revision_no"] = "4" }, RuleWrongType, "$.plan_revision_no"},
	} {
		d := manifestBody()
		tc.mutate(d)
		vs := CheckManifest(parseBody(t, d))
		found := false
		for _, v := range vs {
			found = found || (v.Rule == tc.rule && v.Path == tc.path)
		}
		if !found {
			t.Errorf("%s: want %s at %s, got %v", name, tc.rule, tc.path, vs)
		}
	}
	// no base is allowed (a non_pr node with no repository has none), and no volatile snapshot
	noBase := manifestBody()
	delete(noBase, "base")
	delete(noBase, "volatile")
	if vs := CheckManifest(parseBody(t, noBase)); len(vs) != 0 {
		t.Errorf("a manifest without a base or snapshots: %v", vs)
	}
	// an empty input list is allowed (a node with no incoming edge) and is not the same as a missing one
	d := manifestBody()
	d["inputs"] = []any{}
	if vs := CheckManifest(parseBody(t, d)); len(vs) != 0 {
		t.Errorf("a node with no incoming edge: %v", vs)
	}
}

// A null is a missing value (4.1): a manifest that spells an absent optional field null is accepted and is the same record as one
// that omits it.
func TestManifestNullIsAbsent(t *testing.T) {
	r, s, _ := newRepo(t)
	ctx := context.Background()
	omitted := manifestBody()
	delete(omitted, "base")
	delete(omitted, "volatile")
	delete(omitted["inputs"].([]any)[1].(doc), "artifacts")
	nulled := manifestBody()
	nulled["base"], nulled["volatile"] = nil, nil
	nulled["inputs"].([]any)[1].(doc)["artifacts"] = nil
	nulled["inputs"].([]any)[1].(doc)["head_sha"] = nil
	if vs := CheckManifest(parseBody(t, nulled)); len(vs) != 0 {
		t.Fatalf("null optional fields: %v", vs)
	}
	put := func(d doc) string {
		b, _ := json.Marshal(d)
		digest, err := r.PutManifest(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	first, second := put(omitted), put(nulled)
	if first != second {
		t.Fatalf("a null optional field changed the digest: %s vs %s", first, second)
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM dag_input_manifests").Scan(&n); err != nil || n != 1 {
		t.Fatalf("manifests: %d (%v), want 1: the two spellings are one record", n, err)
	}
	if _, found, err := r.ReadManifest(ctx, first); err != nil || !found {
		t.Fatalf("read back: %v %v", found, err)
	}
}

// A manifest is stored under its digest: the same consumption is one row (whoever records it, in whatever plan), the
// content digest is checked, and a stored body that no longer digests to its key is corruption.
func TestManifestIsContentAddressed(t *testing.T) {
	r, s, _ := newRepo(t)
	ctx := context.Background()
	put := func(d doc) (string, error) {
		b, _ := json.Marshal(d)
		return r.PutManifest(ctx, b)
	}
	first, err := put(manifestBody())
	if err != nil {
		t.Fatal(err)
	}
	// another writer, another plan revision and epoch, the same consumption: one record, the first one kept
	other := manifestBody()
	other["plan_revision_no"], other["coordinator_epoch"], other["created_by_task_id"] = 9, 7, "task-b"
	again, err := put(other)
	if err != nil || again != first {
		t.Fatalf("the same consumption recorded again: %q %v", again, err)
	}
	var n int
	var epoch int
	if err := s.DB.QueryRow("SELECT count(*), max(coordinator_epoch) FROM dag_input_manifests").Scan(&n, &epoch); err != nil || n != 1 || epoch != 2 {
		t.Fatalf("manifests: %d rows, epoch %d (%v): the first record stays", n, epoch, err)
	}
	body, found, err := r.ReadManifest(ctx, first)
	if err != nil || !found || body["manifest_digest"] != first || body["created_by_task_id"] != "task-a" {
		t.Fatalf("read back: %v %v %v", body, found, err)
	}
	if _, found, err := r.ReadManifest(ctx, dig("nothing")); err != nil || found {
		t.Fatalf("an unknown digest: %v %v", found, err)
	}
	// a claimed digest that is not the content's is refused as the manifest tampered
	forged := manifestBody()
	forged["manifest_digest"] = dig("forged")
	if _, err := put(forged); reasonOf(err) != "revision_mismatch" {
		t.Fatalf("a forged digest: %v", err)
	}
	// a body that does not read is rejected before anything is stored
	broken := manifestBody()
	delete(broken, "rule_version")
	var p *PlanRejected
	if _, err := put(broken); !errors.As(err, &p) {
		t.Fatalf("a manifest without a rule version: %v", err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM dag_input_manifests").Scan(&n); err != nil || n != 1 {
		t.Fatalf("manifests after the refusals: %d", n)
	}
	// stored bytes that no longer digest to their key are corruption
	if _, err := s.DB.Exec("UPDATE dag_input_manifests SET body_json = replace(body_json, 'owner/repo', 'elsewhere/repo')"); err != nil {
		t.Fatal(err)
	}
	var corrupt *CorruptError
	if _, _, err := r.ReadManifest(ctx, first); !errors.As(err, &corrupt) {
		t.Fatalf("a tampered manifest read as %v", err)
	}
}
