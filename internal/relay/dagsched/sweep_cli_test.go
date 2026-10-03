package dagsched

import (
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// The sweep through the binary, as the parent runs it (CRW-410): JSON on stdout, exit 0 for an answer and 2 for a refusal with its reason, the tip read through the relay's own tip reader.

// newIntegrationKitAt is newIntegrationKit over a store at a path the binary can open.
func newIntegrationKitAt(t *testing.T, dbPath string) *integrationKit {
	t.Helper()
	f := newFixtureAt(t, dbPath)
	root := t.TempDir()
	settings := map[string]any{"sandbox": map[string]any{"type": "workspaceWrite"}, "approvalPolicy": "never", "cwd": root, "runtimeWorkspaceRoots": []any{root}, "model": "gpt-5", "reasoningEffort": "medium", "environments": []any{}}
	rk := &releaseKit{fixture: f, host: newScriptedHost(t, settings), tips: &tips{sha: head1}, forge: &prs{by: map[string]PullRequest{}}, root: root, marker: t.TempDir(), state: t.TempDir()}
	f.projectParent()
	rk.wire(f.sched)
	k := &integrationKit{releaseKit: rk, repo: newGitRepo(t)}
	k.sched.Tips = mergeturn.TargetReader{}
	k.sched.Ancestry = GitAncestry{}.Ancestry
	k.putPlan("g", 0, "g-r1", addRelNode("I", dag.NodeImplementation), addRelNode("K", dag.NodeNonPR), addRelNode("D", dag.NodeImplementation),
		addEdge("ik", "I", "K", dag.EdgeIntegrated, doc{"target_repository": k.repo.path}))
	return k
}

// cliWorld is a sweep world over a store the binary opens: D accepted at its head, E running in a linked worktree of the repository whose HEAD is its head, I landed on dev; the parent works in the repository.
func cliWorld(t *testing.T) (*sweepWorld, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	k := newIntegrationKitAt(t, filepath.Join(state, "relay.sqlite3"))
	w := buildSweepWorld(t, k)
	w.accept("D")
	k.startNode("g", "E")
	dir := filepath.Join(t.TempDir(), "e")
	k.repo.git("worktree", "add", "-q", "-b", "wt-e", dir, w.head["E"])
	k.exec("UPDATE relationships SET child_cwd = ?, parent_cwd = ? WHERE relationship_id = 'rel-g-E'", dir, k.repo.path)
	k.exec("UPDATE relationships SET parent_cwd = ? WHERE relationship_id = 'rel-g-D'", k.repo.path)
	return w, state
}

func closeStore(t *testing.T, w *sweepWorld) {
	t.Helper()
	if err := w.k.s.Close(); err != nil {
		t.Fatal(err)
	}
}

func memberJSON(t *testing.T, answer map[string]any, kind, left, right string) map[string]any {
	t.Helper()
	for _, m := range answer["members"].([]any) {
		o := m.(map[string]any)
		r, _ := o["right_node_id"].(string)
		if o["kind"] == kind && o["left_node_id"] == left && r == right {
			return o
		}
	}
	t.Fatalf("no %s member %s %s in %v", kind, left, right, answer["members"])
	return nil
}

// A receipt that is not accepted yet (E is running, its report arrived) is swept by the command: E's head is its child's checkout HEAD, D's is its accepted head, and the tip is the one --target names.
func TestCLISweepOfAReceiptBeforeAcceptance(t *testing.T) {
	w, state := cliWorld(t)
	repo := w.k.repo.path
	closeStore(t, w)
	answer := summaryRun(t, state, 0, "dag-conflict-sweep", "--plan", "g", "--actor", "parent", "--repository", repo, "--trigger", "receipt", "--node", "E", "--target", repo+"@dev")
	if answer["schema"] != "dag-conflict-sweep/1" || answer["state"] != "recorded" || answer["trigger"] != "receipt" || answer["trigger_node"] != "E" || answer["repository"] != repo ||
		answer["observed"] != float64(3) || answer["unmeasured"] != float64(0) {
		t.Fatalf("answer = %v", answer)
	}
	pair := memberJSON(t, answer, "pair", "D", "E")
	if pair["status"] != "observed" || pair["conflicts"] != float64(1) || pair["left_head_source"] != "acceptance" || pair["right_head_source"] != "child_checkout" || pair["right_head"] != w.head["E"] {
		t.Errorf("pair = %v", pair)
	}
	if tip := memberJSON(t, answer, "tip", "E", ""); tip["status"] != "observed" || tip["right_head"] != w.tip || tip["left_head_source"] != "child_checkout" {
		t.Errorf("E against the tip = %v", tip)
	}
	// the parent's word on a head, and a refusal with its reason
	named := summaryRun(t, state, 0, "dag-conflict-sweep", "--plan", "g", "--actor", "parent", "--repository", repo, "--target", repo+"@dev", "--head", "E="+w.head["F"])
	if memberJSON(t, named, "pair", "D", "E")["right_head_source"] != "explicit" || named["trigger"] != "manual" || named["sweep_seq"] != float64(2) {
		t.Errorf("named = %v", named)
	}
	if refused := summaryRun(t, state, 2, "dag-conflict-sweep", "--plan", "g", "--actor", "intruder", "--repository", repo); refused["reason"] != "scope_role_mismatch" {
		t.Errorf("refused = %v", refused)
	}
	if _, code := crw(t, state, "dag-conflict-sweep", "--plan", "g", "--actor", "parent", "--head", "nonsense"); code != 4 {
		t.Errorf("a head that is not NODE=SHA: exit %d, want 4", code)
	}
}

// Without --target the tip is the base frozen in the manifest of the node's CURRENT execution. A manifest stored for the same node and never bound (a prepared correction, another plan's) is not it, and no base
// at all leaves the tips unmeasured.
func TestCLISweepTakesTheTipFromTheCurrentManifest(t *testing.T) {
	prepare := func(withBase bool) (*sweepWorld, string, string) {
		w, state := cliWorld(t)
		k := w.k
		var digest string
		if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions WHERE node_id = 'E'").Scan(&digest); err != nil {
			t.Fatal(err)
		}
		// a manifest stored for E and bound to nothing, with another base
		k.exec("INSERT INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES ('prepared-only', 'E', ?, '{}', 0, 'z')",
			"{\"base\":{\"repository\":\"nowhere\",\"ref\":\"nothing\",\"sha\":\"x\"}}")
		if withBase {
			k.exec("UPDATE dag_input_manifests SET body_json = json_set(body_json, '$.base', json(?)) WHERE manifest_digest = ?", "{\"repository\":\""+k.repo.path+"\",\"ref\":\"dev\",\"sha\":\"x\"}", digest)
		}
		closeStore(t, w)
		return w, state, k.repo.path
	}
	t.Run("no base", func(t *testing.T) {
		_, state, repo := prepare(false)
		answer := summaryRun(t, state, 0, "dag-conflict-sweep", "--plan", "g", "--actor", "parent", "--repository", repo, "--node", "E")
		if tip := memberJSON(t, answer, "tip", "E", ""); tip["status"] != "unmeasured" || tip["reason"] != "tip_unreadable" {
			t.Fatalf("no base: %v", tip)
		}
	})
	t.Run("the base of the current execution", func(t *testing.T) {
		w, state, repo := prepare(true)
		answer := summaryRun(t, state, 0, "dag-conflict-sweep", "--plan", "g", "--actor", "parent", "--repository", repo, "--node", "E")
		if tip := memberJSON(t, answer, "tip", "E", ""); tip["status"] != "observed" || tip["right_head"] != w.tip {
			t.Fatalf("the manifest's base: %v", tip)
		}
	})
}

// The landing command carries its sweep in the answer, a repeat finds it done, and dag-conflict-observe carries its drift.
func TestCLICarriesTheSweepAndTheDrift(t *testing.T) {
	w, state := cliWorld(t)
	k := w.k
	repo := k.repo.path
	k.mark(k.acceptNode("g", "I", acceptOpts{HeadSHA: w.head["I"], PR: 30, Forge: "owner/repo", Repository: repo}))
	k.exec("UPDATE relationships SET parent_cwd = ? WHERE relationship_id = 'rel-g-I'", repo)
	closeStore(t, w)
	landed := summaryRun(t, state, 0, "dag-integration-observe", "--plan", "g", "--node", "I", "--actor", "parent")
	sweep := objectOf(t, landed, "conflict_sweep")
	if landed["integrated"] != true || sweep["state"] != "recorded" || sweep["trigger"] != "landing" || sweep["observed"] != float64(3) {
		t.Fatalf("landing = %v", landed)
	}
	again := summaryRun(t, state, 0, "dag-integration-observe", "--plan", "g", "--node", "I", "--actor", "parent")
	if objectOf(t, again, "conflict_sweep")["state"] != "already_swept" {
		t.Fatalf("repeat = %v", again)
	}
	observed := summaryRun(t, state, 0, "dag-conflict-observe", "--plan", "g", "--actor", "parent", "--repository", repo, "--left-node", "D", "--right-node", "E", "--left-head", w.head["D"], "--right-head", w.head["E"])
	if drift, _ := observed["drift"].([]any); len(drift) != 2 || observed["conflicts"] != float64(1) {
		t.Fatalf("observe = %v", observed)
	}
}
