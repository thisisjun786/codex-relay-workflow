//go:build dev

package cxcfuzz

import (
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The goalplan target is registered and its oracle is a Node worker.
func TestGoalplanTargetIsRegistered(t *testing.T) {
	if !slices.Contains(Names(), "goalplan") {
		t.Fatalf("goalplan is not registered: %v", Names())
	}
	target, ok := Lookup("goalplan")
	if !ok {
		t.Fatal("goalplan is not registered")
	}
	if target.Oracle.Command != "node" {
		t.Fatalf("oracle %+v is not a Node worker", target.Oracle)
	}
}

// goalplanGo reads a plan, rewrites it under the write lock, and reports the rewritten bytes with the
// write timestamp masked, all with no Node and no python3.
func TestGoalplanGoReadsAndRewrites(t *testing.T) {
	root := t.TempDir()
	writeGoalplanFile(t, root, `.crw/goalplans/rec-plan/goalplan.json`, `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}}`)
	answer, err := goalplanGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if kind, _ := field(answer, "kind"); kind != "ok" {
		t.Fatalf("a valid plan read as %v", kind)
	}
	written, _ := field(answer, "written")
	text, _ := written.(string)
	if !strings.Contains(text, `"objective": "o"`) || !strings.Contains(text, "@TS@") {
		t.Fatalf("the rewritten bytes %q are not the masked rewrite", written)
	}
}

// A plan whose rewrite would drop a key is refused by the write lock rather than published.
func TestGoalplanGoRefusesALossyRewrite(t *testing.T) {
	root := t.TempDir()
	writeGoalplanFile(t, root, `.crw/goalplans/rec-plan/goalplan.json`, `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "unknownKey": 1}`)
	answer, err := goalplanGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, found := field(answer, "written"); found {
		t.Fatal("a lossy rewrite was published")
	}
	writeError, _ := field(answer, "writeError")
	if text, _ := writeError.(string); !strings.Contains(text, "refusing to rewrite") {
		t.Fatalf("the refusal %q does not name the loss", writeError)
	}
}

// writeGoalplanFile puts one case file under a root.
func writeGoalplanFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// goalplanDoc is one plan document text holding an updatedAt beside a persisted reviewRounds[0].openedAt.
func goalplanDoc(updatedAt, openedAt string) string {
	return "{\n  \"objective\": \"o\",\n  \"updatedAt\": \"" + updatedAt + "\",\n  \"reviewRounds\": [\n    {\n      \"roundId\": \"r1\",\n      \"openedAt\": \"" + openedAt + "\"\n    }\n  ]\n}"
}

// goalplanAnswerText is one plan answer in the real shape: "plan" is the revived form, whose updatedAt
// is the persisted value (readUpdatedAt), and "written" is the published form, whose updatedAt the
// write restamped from its own clock (writeStamp). Both are masked the way goalplanGo masks them; the
// plan reader never stamps the clock, so its read form's updatedAt is never masked.
func goalplanAnswerText(readUpdatedAt, openedAt, writeStamp string) any {
	return maskTimestamps(pyjson.Object{
		{Key: "kind", Value: "ok"},
		{Key: "plan", Value: goalplanDoc(readUpdatedAt, openedAt)},
		{Key: "written", Value: goalplanDoc(writeStamp, openedAt)},
	}, false)
}

// A persisted reviewRounds[0].openedAt is compared as stored: the mask covers only the top-level
// updatedAt the write restamps, so two answers that differ only in the persisted round time are a
// difference rather than a Same (generation 3, c8: the old text-wide mask hid exactly this).
func TestGoalplanCompareSeesAPersistedRoundTimestampChange(t *testing.T) {
	verdict := goalplanCompare(
		goalplanAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z"),
		goalplanAnswerText("2026-05-05T05:05:05.000Z", "2026-02-03T04:05:06.789Z", "2026-06-06T06:06:06.000Z"),
	)
	if verdict.Kind != Differ {
		t.Fatalf("a moved persisted openedAt compared %v (%s), want Differ", verdict.Kind, verdict.Detail)
	}
}

// Two answers that differ only in the write's stamped updatedAt stay Same: the write restamps it from
// the wall clock on both sides, so its two values can never match.
func TestGoalplanCompareIgnoresTheStampedUpdatedAt(t *testing.T) {
	verdict := goalplanCompare(
		goalplanAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z"),
		goalplanAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-09-09T09:09:09.999Z"),
	)
	if verdict.Kind != Same {
		t.Fatalf("two wall-clock updatedAt stamps compared %v (%s), want Same", verdict.Kind, verdict.Detail)
	}
}

// A persisted plan updatedAt is compared as stored too: when the file carried one, both readers keep
// it, so a port that rewrites it to another instant is a difference rather than a Same.
func TestGoalplanCompareSeesAPersistedReadUpdatedAtChange(t *testing.T) {
	verdict := goalplanCompare(
		goalplanAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z"),
		goalplanAnswerText("2026-05-05T05:05:05.001Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z"),
	)
	if verdict.Kind != Differ {
		t.Fatalf("a moved persisted read updatedAt compared %v (%s), want Differ", verdict.Kind, verdict.Detail)
	}
}

// goalplanAnswerWith is one plan answer built from the documents a test gives, masked the way
// goalplanGo masks them.
func goalplanAnswerWith(planText, writtenText string) any {
	return maskTimestamps(pyjson.Object{
		{Key: "kind", Value: "ok"},
		{Key: "plan", Value: planText},
		{Key: "written", Value: writtenText},
	}, false)
}

// A loss below the top level is data-loss for a plan too: the criterion's own example, a Go write
// that loses only workPhases[0].tasks[0].title, is named at its path (CRW-708 generation 5, d1).
func TestGoalplanCompareSeesANestedLoss(t *testing.T) {
	oracleDoc := `{"objective": "o", "workPhases": [{"id": "wp1", "tasks": [{"id": "t1", "title": "a"}]}]}`
	droppedDoc := `{"objective": "o", "workPhases": [{"id": "wp1", "tasks": [{"id": "t1"}]}]}`
	verdict := goalplanCompare(goalplanAnswerWith(droppedDoc, droppedDoc), goalplanAnswerWith(oracleDoc, oracleDoc))
	if verdict.Kind != Differ {
		t.Fatalf("a nested loss compared %v (%s), want Differ", verdict.Kind, verdict.Detail)
	}
	if !strings.Contains(verdict.Detail, "data-loss") || !strings.Contains(verdict.Detail, "workPhases[0].tasks[0].title") {
		t.Fatalf("a nested loss was not named as data-loss at its path: %q", verdict.Detail)
	}
}

// A dropped array element is a loss as well, named by its index (CRW-708 generation 5, d1).
func TestGoalplanCompareSeesADroppedArrayElement(t *testing.T) {
	oracleDoc := `{"objective": "o", "criteria": [{"id": "c-1"}, {"id": "c-2"}]}`
	droppedDoc := `{"objective": "o", "criteria": [{"id": "c-1"}]}`
	verdict := goalplanCompare(goalplanAnswerWith(droppedDoc, droppedDoc), goalplanAnswerWith(oracleDoc, oracleDoc))
	if verdict.Kind != Differ || !strings.Contains(verdict.Detail, "data-loss") || !strings.Contains(verdict.Detail, "criteria[1]") {
		t.Fatalf("a dropped array element compared %v (%s), want a data-loss naming criteria[1]", verdict.Kind, verdict.Detail)
	}
}

// The control: an identical rewrite is Same, and a refused Go write is still named as a refusal rather
// than as a loss (CRW-708 generation 5, d1).
func TestGoalplanCompareKeepsTheRefusalAndTheSame(t *testing.T) {
	same := goalplanAnswerWith(`{"objective": "o"}`, `{"objective": "o"}`)
	if verdict := goalplanCompare(same, same); verdict.Kind != Same {
		t.Fatalf("an identical rewrite compared %v (%s)", verdict.Kind, verdict.Detail)
	}
	refused := maskTimestamps(pyjson.Object{
		{Key: "kind", Value: "ok"},
		{Key: "writeError", Value: "locked: the write lock is held"},
	}, false)
	published := goalplanAnswerWith(`{"objective": "o"}`, `{"objective": "o"}`)
	verdict := goalplanCompare(refused, published)
	if verdict.Kind != Differ || !strings.Contains(verdict.Detail, "refused") {
		t.Fatalf("a refused write compared %v (%s), want the refusal named", verdict.Kind, verdict.Detail)
	}
}

// goalplanInputPlan is one generated input's plan text and whether its fs scenario links the slug
// directory to another directory under the case root.
func goalplanInputPlan(t *testing.T, input any) (string, bool) {
	t.Helper()
	entries, err := fsEntries(input)
	if err != nil {
		t.Fatal(err)
	}
	text, linked := "", false
	for _, entry := range entries {
		switch entry.Kind {
		case "symlink":
			linked = true
		case "file":
			text = entry.Content
		}
	}
	return text, linked
}

// The generator reaches the boundaries the issue body names: duplicate work-phase, task and criterion
// ids, a deep dependsOn chain, and a plan read through a linked slug directory whose target stays
// inside the case root (CRW-708 generation 5, d2).
func TestGoalplanGenerateReachesTheNamedBoundaries(t *testing.T) {
	rng := rand.New(rand.NewSource(708))
	var duplicateIDs, deepChain, linked bool
	linkedInput := any(nil)
	for i := 0; i < 4000 && !(duplicateIDs && deepChain && linked); i++ {
		input := goalplanGenerate(rng, 1)
		text, isLinked := goalplanInputPlan(t, input)
		if strings.Count(text, `"id": "wp1"`) >= 2 && strings.Count(text, `"id": "t1"`) >= 2 &&
			strings.Count(text, `"id": "c-1"`) >= 2 {
			duplicateIDs = true
		}
		if strings.Count(text, `"dependsOn"`) >= 32 {
			deepChain = true
		}
		if isLinked && !linked {
			linked, linkedInput = true, input
		}
	}
	if !duplicateIDs {
		t.Error("no generated plan repeats a work-phase, task and criterion id")
	}
	if !deepChain {
		t.Error("no generated plan holds a deep dependsOn chain")
	}
	if !linked {
		t.Fatal("no generated plan is read through a linked slug directory")
	}
	// The linked shape must be one the harness's own scenario API builds: a target that left the case
	// root would be refused here rather than fuzzed.
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, err := Scenarios(root, linkedInput); err != nil {
		t.Fatalf("the linked slug scenario is refused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".crw", "goalplans", goalplanSlug)); err != nil {
		t.Fatalf("the linked slug directory was not built: %v", err)
	}
}
