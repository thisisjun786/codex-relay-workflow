package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1212 (R04), evaluation round: an agent execution that is current holds the node and its edit regions like any other live execution, in the one reading every release is judged on (Ready); and
// releaseOf, like executionOf, reads a store that holds both kinds as current as neither.

// d1: A (issue CRW-A) and Z (issue CRW-Z) take the same file exclusively. A is paused while Z is released to an agent; resuming A must not make A a candidate for the place Z's agent holds, on either surface.
func TestAgentExecutionHoldsItsExclusiveRegionsInTheReading(t *testing.T) {
	t.Parallel()
	for _, surface := range []string{"agent", "relationship"} {
		t.Run(surface, func(t *testing.T) {
			k := newReleaseKit(t)
			k.putPlan("hp", 0, "hp-r1", issueNode("A", dag.NodeImplementation, "CRW-A"), issueNode("Z", dag.NodeImplementation, "CRW-Z"))
			k.declareRegions("hp", "A", Region{Path: "a/x.go", Kind: "file", Change: "edit", Grade: GradeExclusive})
			k.declareRegions("hp", "Z", Region{Path: "a/x.go", Kind: "file", Change: "edit", Grade: GradeExclusive})
			k.putPlan("hp", 1, "hp-r2", doc{"op": dag.OpPauseNode, "node_id": "A"})
			z := k.mustReleaseAgent("hp", "Z")
			k.putPlan("hp", 2, "hp-r3", doc{"op": dag.OpResumeNode, "node_id": "A"})
			reading := k.read("hp")
			if zr := reading.node("Z"); zr.State != StateRunning || zr.Reason != SkipAlreadyOwned {
				t.Fatalf("Z, held by agent execution %s, reads %+v", z.ExecutionID, zr)
			}
			if ar := reading.node("A"); ar.Reason != DeferEditOverlap || ar.Disposition != DispDefer {
				t.Fatalf("A overlaps the exclusive place Z's agent holds, and reads %+v", ar)
			}
			before := allRows(t, k.s.DB)
			var err error
			if surface == "agent" {
				_, err = k.releaseAgent("hp", "A")
			} else {
				_, err = k.release("hp", "A")
			}
			if err == nil {
				t.Fatal("A was released over the exclusive place an agent execution holds")
			}
			if after := allRows(t, k.s.DB); len(after) != len(before) {
				t.Fatalf("the refused release wrote rows: %v", err)
			}
			// the agent execution is still the node's answer, and its closure frees the place
			if again := k.mustReleaseAgent("hp", "Z"); !again.Replayed || again.ExecutionID != z.ExecutionID {
				t.Fatalf("Z again = %+v", again)
			}
			k.agentCloses("hp", "Z", z.ExecutionID, "cancelled")
			if ar := k.read("hp").node("A"); ar.Disposition != DispReady {
				t.Fatalf("A after the closure of Z's execution reads %+v", ar)
			}
		})
	}
}

// d2: releaseOf refuses a store in which a release of either manifest is current on both sides, under the manifest of either side, the same refusal executionOf makes.
func TestReleaseOfRefusesBothKindsCurrentUnderAnyManifest(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	ctx := context.Background()
	q := k.s.Q(ctx)
	agent := k.mustReleaseAgent("rp", "A")
	other := dig("a manifest of the relationship side")
	k.foreignRelationship("CRW-A")
	k.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES ('rp','A','rel-foreign-CRW-A',1,?,'initial','req-x')", other)
	if _, found, err := executionOf(ctx, q, "rp", "A"); found || err == nil || !strings.Contains(err.Error(), CrossKindExecution) {
		t.Fatalf("executionOf: found=%v %v", found, err)
	}
	for name, manifest := range map[string]string{"the agent's": agent.ManifestDigest, "the relationship's": other, "a third": dig("third")} {
		if r, found, err := releaseOf(ctx, q, "rp", "A", manifest); found || err == nil || !strings.Contains(err.Error(), CrossKindExecution) {
			t.Fatalf("releaseOf under %s manifest: %+v found=%v %v", name, r, found, err)
		}
	}
}
