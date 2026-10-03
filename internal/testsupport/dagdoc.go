package testsupport

import (
	"crypto/sha256"
	"encoding/hex"
)

// The documents of a DAG plan revision (dag-plan-put) that the plan and scheduler tests build the same way.

// Dig is a stand-in for a digest: 64 lowercase hex characters derived from a name.
func Dig(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// NodeDoc is the document of a plan node: its id, the issue it carries, its kind and a criteria digest derived from the id.
func NodeDoc(id, kind string) map[string]any {
	return map[string]any{"node_id": id, "issue_key": "CRW-" + id, "kind": kind, "criteria_set_digest": Dig("criteria " + id)}
}

// LifeOp is a lifecycle change of the revision document: one node's, or (with no node) the plan's.
func LifeOp(op, node string) map[string]any {
	d := map[string]any{"op": op}
	if node != "" {
		d["node_id"] = node
	}
	return d
}
