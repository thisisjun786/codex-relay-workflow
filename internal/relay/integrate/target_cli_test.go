package integrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1304: the commands of the integration target record, driven through the command table and the parser over a temporary store, a temporary
// repository and a temporary bare repository as origin. The store holds what dag-integrate leaves for the integration ref dev-int (an intent whose
// base is the first commit and a ref_moved stage onto the second), written through the typed stage writer.

type targetEnv struct {
	t        *testing.T
	state    string
	repo     string
	remote   string
	base     string
	moved    string
	foreign  string
	services dispatch.Services
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newTargetEnv(t *testing.T) *targetEnv {
	t.Helper()
	testsupport.SandboxAccountHomes(t)
	e := &targetEnv{t: t, state: filepath.Join(t.TempDir(), "state"), repo: t.TempDir(), remote: t.TempDir()}
	gitIn(t, e.remote, "init", "-q", "--bare")
	gitIn(t, e.repo, "init", "-q", "-b", "dev")
	write := func(name string) string {
		if err := os.WriteFile(filepath.Join(e.repo, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, e.repo, "add", name)
		gitIn(t, e.repo, "commit", "-q", "-m", name)
		return gitIn(t, e.repo, "rev-parse", "HEAD")
	}
	e.base = write("base.txt")
	gitIn(t, e.repo, "remote", "add", "origin", e.remote)
	gitIn(t, e.repo, "push", "-q", "origin", "dev:refs/heads/dev")
	gitIn(t, e.repo, "checkout", "-q", "-b", "dev-int")
	e.moved = write("moved.txt")
	gitIn(t, e.repo, "checkout", "-q", "-b", "foreign", e.base)
	e.foreign = write("foreign.txt")
	gitIn(t, e.repo, "checkout", "-q", "dev-int")

	db := filepath.Join(e.state, "relay.sqlite3")
	testsupport.Create(t, db, "", "go")
	ctx := context.Background()
	s, err := store.Open(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_plans VALUES ('g','P-TEST','task-test','2026-10-14T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	intent, err := json.Marshal(map[string]string{"integration_ref": "dev-int", "base": e.base, "verified_head": e.moved, "verification_digest": "sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []store.IntegrationStageRow{
		{StageID: "s1", BatchID: "b1", PlanID: "g", Stage: "intent", Detail: string(intent), RecordedBy: "parent", RecordedAt: "2026-10-14T00:00:00Z"},
		{StageID: "s2", BatchID: "b1", PlanID: "g", Stage: "ref_moved", Detail: e.moved, HeadSHA: e.moved, RecordedBy: "parent", RecordedAt: "2026-10-14T00:00:01Z"},
	} {
		if err := store.RecordIntegrationStage(ctx, s, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	e.services = dispatch.Services{Selection: store.StateSelection{Path: e.state, Source: "flag"}}
	return e
}

// run is one command line through the command table and the parser; it answers the printed document.
func (e *targetEnv) run(command string, flags ...string) (map[string]any, error) {
	e.t.Helper()
	registered, ok := dispatch.Lookup(command)
	if !ok {
		e.t.Fatalf("%s is not registered", command)
	}
	parsed := argparse.Parse(command, flags)
	if parsed.Message != "" {
		e.t.Fatalf("%s %v: %s", command, flags, parsed.Message)
	}
	out, err := registered.Run(context.Background(), e.services, dispatch.Args{Parsed: parsed, Defaults: registered.Defaults})
	if err != nil {
		return nil, err
	}
	var text bytes.Buffer
	if err := contract.Emit(&text, out); err != nil {
		e.t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(text.Bytes(), &doc); err != nil {
		e.t.Fatalf("%s printed %q: %v", command, text.String(), err)
	}
	return doc, nil
}

func (e *targetEnv) rows() []map[string]string {
	e.t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(e.state, "relay.sqlite3"), "")
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.DB.QueryContext(ctx, "SELECT kind, integration_ref, remote, remote_ref, observed_head, source, recorded_by FROM dag_integration_targets ORDER BY rowid")
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var kind, ref, remote, remoteRef, head, source, by string
		if err := rows.Scan(&kind, &ref, &remote, &remoteRef, &head, &source, &by); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, map[string]string{"kind": kind, "ref": ref, "remote": remote, "remote_ref": remoteRef, "head": head, "source": source, "by": by})
	}
	return out
}

func (e *targetEnv) pushArgs() []string {
	return []string{"--checkout", e.repo, "--integration-ref", "dev-int", "--remote", "origin", "--remote-ref", "dev"}
}

func refusalOf(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

// c1: the record command appends a row once, answers recorded false for a repeat and true for a change, and refuses an unknown integration ref
// and a dash as input errors.
func TestTargetRecordCommand(t *testing.T) {
	e := newTargetEnv(t)
	flags := append(e.pushArgs(), "--actor", "parent")
	first, err := e.run("dag-integration-target-record", flags...)
	if err != nil || first["schema"] != SchemaTargetRecord || first["recorded"] != true || first["plan_id"] != "g" || first["integration_ref"] != "dev-int" || first["recorded_by"] != "parent" {
		t.Fatalf("first record: %v, %v", first, err)
	}
	again, err := e.run("dag-integration-target-record", flags...)
	if err != nil || again["recorded"] != false {
		t.Fatalf("a repeat: %v, %v; want recorded false", again, err)
	}
	changed, err := e.run("dag-integration-target-record", append(append([]string{}, flags...), "--plan", "g2")...)
	if err != nil || changed["recorded"] != false {
		t.Fatalf("a repeat that names another plan: %v, %v; want a no-op", changed, err)
	}
	moved := append([]string{}, flags...)
	moved[7] = "main" // --remote-ref
	if doc, err := e.run("dag-integration-target-record", moved...); err != nil || doc["recorded"] != true || doc["remote_ref"] != "main" {
		t.Fatalf("a changed remote ref: %v, %v; want recorded true", doc, err)
	}
	if rows := e.rows(); len(rows) != 2 || rows[0]["source"] != "record" {
		t.Fatalf("rows %v; want the two target rows", rows)
	}
	for name, bad := range map[string][]string{
		"an unknown integration ref": {"--checkout", e.repo, "--integration-ref", "nope-int", "--remote", "origin", "--remote-ref", "dev", "--actor", "parent"},
		"a remote with a dash":       {"--checkout", e.repo, "--integration-ref", "dev-int", "--remote=-origin", "--remote-ref", "dev", "--actor", "parent"},
	} {
		if _, err := e.run("dag-integration-target-record", bad...); refusalOf(err) != "merge_target_unreadable" {
			t.Errorf("%s: %v; want a merge_target_unreadable refusal", name, err)
		}
	}
	if rows := e.rows(); len(rows) != 2 {
		t.Fatalf("refusals wrote rows: %v", rows)
	}
}

// c1 and c3: dag-integrate-push records the target and the head it read, prints what it printed before, and the second push is compared with the
// recorded target's own heads. The checker then reads the remote: on the path, then moved by a commit pushed outside the integration path.
func TestPushRecordsTargetAndCheckerReadsIt(t *testing.T) {
	e := newTargetEnv(t)
	pushed, err := e.run("dag-integrate-push", e.pushArgs()...)
	if err != nil || pushed["outcome"] != "pushed" || pushed["remote_head"] != e.moved {
		t.Fatalf("push: %v, %v; want pushed onto %s", pushed, err, e.moved)
	}
	for _, key := range []string{"target_moved", "target_check_error", "target_record_error"} {
		if _, ok := pushed[key]; ok {
			t.Fatalf("the push printed %s: %v", key, pushed)
		}
	}
	rows := e.rows()
	if len(rows) != 2 || rows[0]["kind"] != "target" || rows[0]["source"] != "push" || rows[0]["by"] != dagsched.PushActor || rows[1]["kind"] != "observed" || rows[1]["head"] != e.moved {
		t.Fatalf("rows after the push: %v; want the target and the head %s", rows, e.moved)
	}
	if again, err := e.run("dag-integrate-push", e.pushArgs()...); err != nil || again["outcome"] != "up_to_date" || len(e.rows()) != 2 {
		t.Fatalf("second push: %v, %v with %d rows; want up_to_date and the same two rows", again, err, len(e.rows()))
	}

	check, err := e.run("dag-integration-target-check")
	targets, _ := check["targets"].([]any)
	if err != nil || check["schema"] != SchemaTargetCheck || check["checked"] != float64(1) || check["moved"] != float64(0) || len(targets) != 1 || targets[0].(map[string]any)["status"] != "on_path" {
		t.Fatalf("check on the path: %v, %v", check, err)
	}
	// a commit pushed outside the integration path: the remote moves to the foreign commit, which the relay never recorded
	gitIn(t, e.repo, "push", "-q", "-f", "origin", e.foreign+":refs/heads/dev")
	moved, err := e.run("dag-integration-target-check", "--integration-ref", "dev-int")
	targets, _ = moved["targets"].([]any)
	if err != nil || moved["moved"] != float64(1) || len(targets) != 1 {
		t.Fatalf("check after a foreign push: %v, %v", moved, err)
	}
	entry := targets[0].(map[string]any)
	detail, _ := entry["moved"].(map[string]any)
	if entry["status"] != "moved" || entry["remote_head"] != e.foreign || detail["fault_class"] != dagsched.ClassIntegrationTargetMoved || detail["fault_id"] == nil || entry["observed"] != true {
		t.Fatalf("reading of the foreign push: %v", entry)
	}
	// the push is refused as before (the remote holds a commit the branch does not contain), and it records nothing new about the target
	if _, err := e.run("dag-integrate-push", e.pushArgs()...); refusalOf(err) != "merge_base_mismatch" || !strings.Contains(err.Error(), dagsched.ClassIntegrationTargetMoved) {
		t.Fatalf("push over the foreign commit: %v; want merge_base_mismatch naming the moved target", err)
	}
	// input errors of the checker
	if _, err := e.run("dag-integration-target-check", "--integration-ref", "nope-int"); refusalOf(err) != "merge_target_unreadable" {
		t.Fatalf("an unknown integration ref: %v; want a merge_target_unreadable refusal", err)
	}
	if _, err := e.run("dag-integration-target-check", "--checkout=-C"); refusalOf(err) != "merge_target_unreadable" {
		t.Fatalf("a checkout with a dash: %v; want a merge_target_unreadable refusal", err)
	}
}

// dag-integrate gives --remote and --remote-ref together or not at all; the check runs before the store is opened or anything is merged.
func TestBatchTargetFlagsComeTogether(t *testing.T) {
	e := newTargetEnv(t)
	line := []string{"--plan", "g", "--actor", "parent", "--checkout", e.repo, "--integration-ref", "dev-int", "--base", "dev", "--verify", "true"}
	for name, extra := range map[string][]string{"only --remote": {"--remote", "origin"}, "only --remote-ref": {"--remote-ref", "dev"}} {
		_, err := e.run("dag-integrate", append(append([]string{}, line...), extra...)...)
		var usage *dispatch.UsageError
		if !errors.As(err, &usage) {
			t.Errorf("%s: %v; want a usage error", name, err)
		}
	}
	if _, err := e.run("dag-integrate", append(append([]string{}, line...), "--remote=-origin", "--remote-ref", "dev")...); refusalOf(err) != "merge_target_unreadable" {
		t.Errorf("a remote with a dash: %v; want a merge_target_unreadable refusal before the batch", err)
	}
	if rows := e.rows(); len(rows) != 0 {
		t.Fatalf("refused lines wrote rows: %v", rows)
	}
}

// The batch records its target through the same record; the answer rides in the batch's document as target and a record that cannot be written is
// named there, never a refusal of the batch.
func TestRecordBatchTargetNamesWhatItRecorded(t *testing.T) {
	e := newTargetEnv(t)
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(e.state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	sched := &dagsched.Scheduler{Store: s}
	in := dagsched.TargetInput{Checkout: e.repo, IntegrationRef: "dev-int", Remote: "origin", RemoteRef: "dev", Actor: "parent", Source: store.TargetSourceIntegrate}
	doc := recordBatchTarget(ctx, sched, &batchTargetInput{in: in}, contract.OrderedObject{{Key: "ok", Value: true}})
	var text bytes.Buffer
	if err := contract.Emit(&text, doc); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(text.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	target, _ := got["target"].(map[string]any)
	if target["recorded"] != true || target["error"] != nil || target["plan_id"] != "g" {
		t.Fatalf("target %v; want recorded for plan g", target)
	}
	bad := in
	bad.IntegrationRef = "nope-int"
	doc = recordBatchTarget(ctx, sched, &batchTargetInput{in: bad}, contract.OrderedObject{{Key: "ok", Value: true}})
	text.Reset()
	if err := contract.Emit(&text, doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), `"recorded": false`) || !strings.Contains(text.String(), "nope-int") {
		t.Fatalf("a record that failed: %s; want recorded false and the reason", text.String())
	}
	if untouched := recordBatchTarget(ctx, sched, nil, contract.OrderedObject{{Key: "ok", Value: true}}); len(untouched) != 1 {
		t.Fatalf("a batch with no target flags changed its document: %v", untouched)
	}
}

// The documents carry their schema and the reading's fields; a target that was deferred says why and a moved one names the fault.
func TestTargetCheckDocumentShape(t *testing.T) {
	readings := []dagsched.TargetReading{
		{Target: store.IntegrationTargetRow{Checkout: "/c", IntegrationRef: "dev-int", Remote: "origin", RemoteRef: "dev"}, Status: dagsched.TargetUnreachable, Detail: "could not resolve host"},
		{Target: store.IntegrationTargetRow{Checkout: "/c", IntegrationRef: "other-int", Remote: "origin", RemoteRef: "dev"}, Status: dagsched.TargetMovedOutside, RemoteHead: "abc", Observed: true,
			Moved: &dagsched.TargetMoved{FaultID: "f-1", Expected: []string{"h1"}, ExpectedTotal: 1}, ObservationError: "busy"},
	}
	var out bytes.Buffer
	if err := contract.Emit(&out, targetCheckObject(readings)); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	targets := got["targets"].([]any)
	first, second := targets[0].(map[string]any), targets[1].(map[string]any)
	if got["ok"] != true || got["checked"] != float64(2) || got["moved"] != float64(1) || first["status"] != "unreachable" || first["remote_head"] != nil || first["detail"] != "could not resolve host" || first["moved"] != nil {
		t.Fatalf("document %s", out.String())
	}
	if second["moved"].(map[string]any)["fault_id"] != "f-1" || second["observation_error"] != "busy" || second["remote_head"] != "abc" {
		t.Fatalf("second target %v", second)
	}
}
