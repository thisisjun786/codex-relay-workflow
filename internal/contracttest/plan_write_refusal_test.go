package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1212 (R04; the P3 of the cancelled CRW-1003): contract/README.md names the refusals of a plan write, and the names are the ones the code gives. The README line is read from the file, and the
// refusals are produced by the code: a plan revision that breaks the plan rules, one that loses to another writer, and one from a session that holds no coordinator epoch are each put to a real store,
// and the reason each is refused with is read from the refusal itself.

func putRevision(t *testing.T, repo *dag.Repo, plan, request string, parent int, epoch int, changes []any) error {
	t.Helper()
	document := map[string]any{"schema": dag.SchemaRevision, "plan_id": plan, "project_key": "P-TEST", "request_id": request, "expected_parent_revision": parent, "author_task_id": "task-test", "changes": changes}
	if epoch != 0 {
		document["coordinator_epoch"] = epoch
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, err = repo.Put(context.Background(), rev)
	return err
}

func refusalOf(t *testing.T, err error) string {
	t.Helper()
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("not a refusal: %v", err)
	}
	return refused.Reason
}

func TestContractReadmeNamesPlanWriteRefusal(t *testing.T) {
	t.Parallel()
	// the refusals the code gives a plan write
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo := &dag.Repo{Store: s, Now: func() string { return "2026-10-11T00:00:00.000000+00:00" }}
	node := []any{map[string]any{"op": dag.OpAddNode, "node": testsupport.NodeDoc("A", dag.NodeNonPR)}}
	given := map[string]string{}
	// a revision that breaks the plan rules: an edge between nodes the plan does not hold
	given["rejected revision"] = refusalOf(t, putRevision(t, repo, "p1", "r-bad", 0, 0, []any{map[string]any{"op": dag.OpAddEdge, "edge": map[string]any{"edge_id": "e1", "from_node_id": "X", "to_node_id": "Y", "kind": dag.EdgeArtifactVerified}}}))
	if err := putRevision(t, repo, "p1", "r-1", 0, 0, node); err != nil {
		t.Fatal(err)
	}
	// a writer that read the plan before another one wrote
	given["lost expected parent"] = refusalOf(t, putRevision(t, repo, "p1", "r-2", 0, 0, []any{map[string]any{"op": dag.OpAddNode, "node": testsupport.NodeDoc("B", dag.NodeNonPR)}}))
	// a session that names an epoch nobody claimed
	given["epoch nobody holds"] = refusalOf(t, putRevision(t, repo, "p1", "r-3", 1, 7, []any{map[string]any{"op": dag.OpAddNode, "node": testsupport.NodeDoc("C", dag.NodeNonPR)}}))
	var code []string
	for _, reason := range given {
		code = append(code, reason)
	}
	sort.Strings(code)
	if got := strings.Join(code, ","); got != "malformed_receipt,plan_revision_conflict,stale_coordinator_epoch" {
		t.Fatalf("the code refuses a plan write with %v (%v)", code, given)
	}

	// the names the README gives
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(filepath.Join(root, "contract", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(l, "Plan-write refusals:") {
			if line != "" {
				t.Fatal("contract/README.md has two lines that name the plan-write refusals")
			}
			line = l
		}
	}
	if line == "" {
		t.Fatal(`contract/README.md has no line that starts with "Plan-write refusals:"`)
	}
	// the names before the semicolon are the refusals; what follows is about the readers of a plan
	sentence, _, _ := strings.Cut(line, ";")
	var named []string
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(sentence, -1) {
		if m[1] != "dag_plan_put" && m[1] != "dag-plan-put" {
			named = append(named, m[1])
		}
	}
	sort.Strings(named)
	if strings.Join(named, ",") != strings.Join(code, ",") {
		t.Fatalf("contract/README.md names the plan-write refusals %v; the code gives %v", named, code)
	}
	// the closed reasons the agent surface adds are named in the README as the code spells them, and none of them is a refusal reason of the contract
	exitCodes, err := os.ReadFile(filepath.Join(root, "contract", "schema", "relay-exit-codes.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{dagsched.LiveRelationshipExecution, dagsched.LiveAgentExecution, dagsched.CrossKindExecution, dagsched.RegionOwnerMissing, dagsched.RegionOwnerAmbiguous} {
		if !strings.Contains(string(readme), "`"+reason+"`") {
			t.Errorf("contract/README.md does not name the closed reason %s", reason)
		}
		if strings.Contains(string(exitCodes), `"`+reason+`"`) {
			t.Errorf("%s was registered as a refusal reason; the agent surface registers none", reason)
		}
	}
}
