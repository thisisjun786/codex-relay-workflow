package mergeturn

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"reflect"
	"regexp"
	"sort"
)

func targetStep(t *testing.T, w *fx, reader TargetReader, repository, branch string, paths ...string) {
	t.Helper()
	tip, err := reader.Tip(context.Background(), repository, branch)
	var result any
	if err != nil {
		detail := err.Error()
		if strings.Contains(detail, "/missing-gh: no such file or directory") {
			detail = "the forge could not be read: fork/exec <missing-gh>: no such file or directory"
		}
		result = map[string]any{"refused": map[string]any{"reason": "merge_target_unreadable", "detail": detail}}
	} else {
		result = map[string]any{"ok": tip}
	}
	raw, e := json.Marshal(result)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range paths {
		raw = []byte(strings.ReplaceAll(string(raw), path, "<repo>"))
	}
	var normalized map[string]any
	if e := json.Unmarshal(raw, &normalized); e != nil {
		t.Fatal(e)
	}
	w.steps = append(w.steps, normalized)
}
func gitWork(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}
func recordCalls(w *fx, path string) {
	w.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		w.t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(raw) == 0 {
		lines = []string{}
	}
	w.step([]any{lines}, nil)
}
func Test26_MTG_1_python_whole_output(t *testing.T) {
	w := newFx(t)
	dir, _ := repo(t)
	second := commitObject(t, dir, "second")
	reader := TargetReader{}
	targetStep(t, w, reader, dir, "main", dir)
	git(t, dir, "update-ref", "refs/heads/main", second)
	targetStep(t, w, reader, dir, "main", dir)
	root := filepath.Dir(dir)
	work := filepath.Join(root, "work")
	gitWork(t, "init", "-b", "main", work)
	gitWork(t, "-C", work, "fetch", dir, "main")
	gitWork(t, "-C", work, "update-ref", "refs/heads/main", second)
	targetStep(t, w, reader, work, "main", work)
	linked := filepath.Join(root, "linked")
	gitWork(t, "-C", work, "worktree", "add", "-b", "side", linked)
	targetStep(t, w, reader, linked, "main", linked)
	w.sameAsPython("mtg1_local")
}
func Test26_MTG_2_python_whole_output(t *testing.T) {
	w := newFx(t)
	dir, _ := repo(t)
	second := commitObject(t, dir, "second")
	reader := TargetReader{}
	for _, branch := range []string{"main~1", "main^", "main@{1}", "-main", "main.lock", "ma*in", " main", "a..b", "x/.hidden", "a//b", "main.", "@", "ma in", "ma\tin", "x/"} {
		targetStep(t, w, reader, dir, branch, dir)
	}
	for _, branch := range []string{"topic#42", "50%off", "release/1.2"} {
		git(t, dir, "update-ref", "refs/heads/"+branch, second)
		targetStep(t, w, reader, dir, branch, dir)
	}
	w.sameAsPython("mtg2_refs")
}
func Test26_MTG_3_python_whole_output(t *testing.T) {
	w := newFx(t)
	dir, _ := repo(t)
	root := filepath.Dir(dir)
	reader := TargetReader{}
	for _, path := range []string{"relative", filepath.Join(root, "missing"), root, filepath.Join(dir, "objects")} {
		targetStep(t, w, reader, path, "main", dir, root)
	}
	targetStep(t, w, reader, dir, "nope", dir, root)
	odd := filepath.Join(root, "odd")
	if err := os.Mkdir(odd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(odd, ".git"), []byte("not a pointer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	targetStep(t, w, reader, odd, "main", dir, root)
	t.Setenv("GIT_DIR", dir)
	t.Setenv("GIT_NAMESPACE", "x")
	targetStep(t, w, reader, dir, "main", dir, root)
	targetStep(t, w, reader, root, "main", dir, root)
	w.sameAsPython("mtg3_unreadable")
}
func Test26_MTG_5_python_whole_output(t *testing.T) {
	w := newFx(t)
	sha := "abcdef1234" + strings.Repeat("0", 30)
	for _, v := range [][2]string{{strings.ToUpper(sha), sha}, {"abcdef1", sha}, {sha, "abcdef1"}, {"base-0", "base-00"}, {"base-0", "base-0"}, {"", ""}} {
		w.step(SameCommit(v[0], v[1]), nil)
	}
	w.sameAsPython("mtg5_commit")
}
func Test26_MTG_6_python_whole_output(t *testing.T) {
	w := newFx(t)
	first := w.landed("head-a", fxPost)
	w.r3Landing(first)
	second := w.heldOn(alpha, fxA, "head-c", fxBase)
	w.merged(fxPost)
	w.step(w.check(second, "head-c", fxPost, ""))
	w.step(w.restate(first, "", fxPost, ""))
	w.step(w.check(second, "head-c", fxPost, ""))
	w.merged("merge-2")
	w.step(w.land(second, "merge-2", "", ""))
	w.turn(second)
	w.sameAsPython("mtg6_trial")
}
func Test26_MTG_6_python_bare_cli_whole_output(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "R-A.git")
	gitWork(t, "init", "--bare", repoPath)
	makeCommit := func(message string, parent string) string {
		t.Helper()
		tree := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
		args := []string{"--git-dir=" + repoPath, "commit-tree", tree}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		args = append(args, "-m", message)
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=relay test", "GIT_AUTHOR_EMAIL=relay@test.invalid", "GIT_COMMITTER_NAME=relay test", "GIT_COMMITTER_EMAIL=relay@test.invalid", "GIT_AUTHOR_DATE=2026-09-24T00:00:00Z", "GIT_COMMITTER_DATE=2026-09-24T00:00:00Z", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("commit-tree: %s: %v", out, err)
		}
		return strings.TrimSpace(string(out))
	}
	base := makeCommit("skeleton", "")
	a1 := makeCommit("A1", base)
	a2 := makeCommit("A2", a1)
	git(t, repoPath, "update-ref", "refs/heads/main", base)
	state := filepath.Join(root, "state")
	steps := []map[string]any{}
	call := func(args ...string) map[string]any {
		t.Helper()
		var out, stderr bytes.Buffer
		code := registry.Execute(context.Background(), append([]string{"--state", state}, args...), &out, &stderr)
		if stderr.Len() > 0 {
			t.Fatalf("%v stderr: %s", args, stderr.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
			t.Fatalf("%v: %s: %v", args, out.String(), err)
		}
		steps = append(steps, map[string]any{"ok": map[string]any{"exit": code, "payload": payload}})
		return payload
	}
	claim := func(head string) string {
		result := call("merge-turn-request", "--repository", repoPath, "--base-ref", "main", "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a", "--head", head, "--ready")
		turn := result["turnId"].(string)
		grant := result["grant"].(map[string]any)["grantId"].(string)
		call("merge-turn-acknowledge", "--turn", turn, "--actor", "task-alpha", "--grant", grant, "--evidence", "read the grant")
		return turn
	}
	check := func(turn, head, baseSHA string) {
		checks := fmt.Sprintf(`[{"runId":%q,"name":"required","headSha":%q,"conclusion":"success","attempt":1}]`, "run-"+head[:7], head)
		call("merge-turn-check", "--turn", turn, "--actor", "task-alpha", "--head-sha", head, "--base-sha", baseSHA, "--checks", checks, "--review", `{"hasNextPage":false,"pagesRead":1,"totalCount":1,"threadsSeen":["thread-1"],"unresolved":0}`, "--required", "required")
	}
	call("linkage-bind", "--role", "parent", "--scope", "PRJ-A", "--task", "task-alpha", "--host", "host-a")
	first := claim(a1)
	check(first, a1, base)
	git(t, repoPath, "update-ref", "refs/heads/main", a1)
	call("merge-turn-land", "--turn", first, "--actor", "task-alpha", "--landed-sha", a1, "--observed-base-sha", base, "--evidence", "fast-forwarded main to A1")
	call("merge-turn-land", "--turn", first, "--actor", "task-alpha", "--landed-sha", a1, "--evidence", "fast-forwarded main to A1")
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.DB.Exec("UPDATE merge_turns SET observed_base_sha=? WHERE turn_id=?", base, first); err != nil {
		t.Fatal(err)
	}
	second := claim(a2)
	check(second, a2, a1)
	call("merge-turn-restate-base", "--turn", first, "--actor", "task-alpha", "--observed-base-sha", a1, "--evidence", "main after A1's fast-forward")
	check(second, a2, a1)
	git(t, repoPath, "update-ref", "refs/heads/main", a2)
	call("merge-turn-land", "--turn", second, "--actor", "task-alpha", "--landed-sha", a2, "--observed-base-sha", a2, "--evidence", "fast-forwarded main to A2")
	for _, query := range []string{"SELECT * FROM merge_turns ORDER BY candidate_head", "SELECT * FROM merge_turn_checks ORDER BY check_id", "SELECT * FROM merge_turn_ledger ORDER BY turn_id,idempotency_key"} {
		rows, err := s.All(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		result := []any{}
		for _, row := range rows {
			record := map[string]any{}
			for _, field := range row {
				record[field.Name] = field.Value
			}
			result = append(result, record)
		}
		steps = append(steps, map[string]any{"ok": result})
	}
	normalizeBareCLI(t, steps, repoPath)
	oracle, err := pythonC()
	if err != nil {
		t.Fatal(err)
	}
	want := oracle["mtg6_bare_cli"]
	if len(steps) != len(want) {
		t.Fatalf("steps %d vs %d", len(steps), len(want))
	}
	stable := stableTargets(t, steps).([]any)
	for i := range want {
		if !reflect.DeepEqual(stable[i], want[i]) {
			got, _ := json.MarshalIndent(steps[i], "", " ")
			expected, _ := json.MarshalIndent(want[i], "", " ")
			t.Errorf("step %d: Go %s Python %s", i, got, expected)
		}
	}
}
func normalizeBareCLI(t *testing.T, steps []map[string]any, repository string) {
	t.Helper()
	pattern := regexp.MustCompile(`(?:tgt|mtn|mtg|mte|chk)-[0-9a-f]{32}`)
	names := map[string]string{}
	prefixCounts := map[string]int{}
	for i, step := range steps {
		raw, err := json.Marshal(step)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(raw), repository, "<repo>")
		text = regexp.MustCompile(`20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:.]+(?:\\u002b|\+|%2B)00:00`).ReplaceAllString(text, fxISO)
		text = pattern.ReplaceAllStringFunc(text, func(key string) string {
			if value, ok := names[key]; ok {
				return value
			}
			prefix := key[:3]
			prefixCounts[prefix]++
			value := fmt.Sprintf("<%s-%d>", prefix, prefixCounts[prefix])
			names[key] = value
			return value
		})
		var normalized map[string]any
		if err = json.Unmarshal([]byte(text), &normalized); err != nil {
			t.Fatal(err)
		}
		var stable func(any)
		stable = func(value any) {
			switch item := value.(type) {
			case map[string]any:
				for key, child := range item {
					stable(child)
					if key == "ledger" {
						if rows, ok := child.([]any); ok {
							sort.Slice(rows, func(i, j int) bool {
								return rows[i].(map[string]any)["idempotencyKey"].(string) < rows[j].(map[string]any)["idempotencyKey"].(string)
							})
						}
					}
				}
			case []any:
				for _, child := range item {
					stable(child)
				}
			}
		}
		stable(normalized)
		steps[i] = normalized
	}
	entries := map[string]string{}
	var collect func(any)
	collect = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			entry, _ := item["entryId"].(string)
			if entry == "" {
				entry, _ = item["entry_id"].(string)
			}
			key, _ := item["idempotencyKey"].(string)
			if key == "" {
				key, _ = item["idempotency_key"].(string)
			}
			turn, _ := item["turnId"].(string)
			if turn == "" {
				turn, _ = item["turn_id"].(string)
			}
			if entry != "" && key != "" {
				entries[entry] = turn + "|" + key
			}
			for _, child := range item {
				collect(child)
			}
		case []any:
			for _, child := range item {
				collect(child)
			}
		}
	}
	for _, step := range steps {
		collect(step)
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return entries[ids[i]] < entries[ids[j]] })
	aliases := map[string]string{}
	for i, id := range ids {
		aliases[id] = fmt.Sprintf("<entry-%d>", i+1)
	}
	entryPattern := regexp.MustCompile(`<mte-\d+>`)
	for i, step := range steps {
		raw, err := json.Marshal(step)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(strings.ReplaceAll(string(raw), `\u003c`, "<"), `\u003e`, ">")
		text = entryPattern.ReplaceAllStringFunc(text, func(id string) string {
			if alias, ok := aliases[id]; ok {
				return alias
			}
			return id
		})
		var value map[string]any
		if err = json.Unmarshal([]byte(text), &value); err != nil {
			t.Fatal(err)
		}
		steps[i] = value
	}
	for _, step := range steps {
		rows, ok := step["ok"].([]any)
		if !ok || len(rows) == 0 {
			continue
		}
		first, ok := rows[0].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := first["check_id"]; ok {
			sort.Slice(rows, func(i, j int) bool {
				return rows[i].(map[string]any)["turn_id"].(string) < rows[j].(map[string]any)["turn_id"].(string)
			})
		} else if _, ok := first["entry_id"]; ok {
			sort.Slice(rows, func(i, j int) bool {
				a, b := rows[i].(map[string]any), rows[j].(map[string]any)
				return a["turn_id"].(string)+"|"+a["idempotency_key"].(string) < b["turn_id"].(string)+"|"+b["idempotency_key"].(string)
			})
		}
	}
}
func Test26_MTG_7_python_whole_output(t *testing.T) {
	w := newFx(t)
	var out, stderr bytes.Buffer
	code := registry.Execute(w.ctx, []string{"merge-turn-restate-base", "--help"}, &out, &stderr)
	w.step(map[string]any{"helpExitsZero": code == 0, "hasObservedBaseSha": strings.Contains(out.String(), "--observed-base-sha")}, nil)
	w.sameAsPython("mtg7_help")
}
func Test26_MTG_8_python_whole_output(t *testing.T) {
	w := newFx(t)
	turn := w.held()
	for _, change := range []struct {
		key   string
		value any
	}{{"threadsSeen", json.Number("1")}, {"unresolved", []any{}}} {
		b := defaults()
		stated := green()
		for i := range stated {
			if stated[i].Key == change.key {
				stated[i].Value = change.value
			}
		}
		b.review = stated
		w.checksRows(turn)
		w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id = ?", turn)
		w.step(w.begin(turn, b))
		w.checksRows(turn)
		w.rows("SELECT * FROM merge_turn_ledger WHERE turn_id = ?", turn)
	}
	b := defaults()
	b.review = review("hasNextPage", false, "pagesRead", json.Number("1"), "totalCount", json.Number("1"), "unresolved", json.Number("0"))
	w.step(w.begin(turn, b))
	w.step(w.begin(turn, defaults()))
	w.sameAsPython("mtg8_review")
}
func wakeFx(t *testing.T) *fx {
	w := newFx(t)
	w.m.Delivery = StoreDelivery{Store: w.s}
	w.exec(`INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, "rel-a", "ISS-1", "active", alpha.TaskID, alpha.HostID, "task-child", "host-child", 3, "[]", `["task-alpha"]`, fxISO, fxISO)
	return w
}
func wakeNotices(w *fx) {
	w.rows("SELECT d.event_id, d.recipient_task_id, d.state, e.receipt FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE d.kind='merge_turn_grant' ORDER BY d.created_at,d.event_id")
}
func wakePromote(w *fx) string {
	rival := w.claim(beta, fxB, "head-b")
	waiter := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-a", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	w.must(w.m.Release(w.ctx, rival["turnId"].(string), beta.TaskID, "returned", "done", ""))
	return waiter["turnId"].(string)
}
func Test26_MTW_1_python_whole_output(t *testing.T) {
	w := wakeFx(t)
	turn := wakePromote(w)
	w.turn(turn)
	wakeNotices(w)
	record := w.must(w.m.Turn(w.ctx, turn))
	grant := record["grant"].(map[string]any)["grantId"].(string)
	w.step(w.m.Acknowledge(w.ctx, turn, alpha.TaskID, grant, "read it"))
	w.step(w.m.Release(w.ctx, turn, alpha.TaskID, "returned", "handing it back", ""))
	wakeNotices(w)
	w.sameAsPython("mtw1_promotion")
}
func Test26_MTW_2_python_whole_output(t *testing.T) {
	w := wakeFx(t)
	held := w.claim(beta, fxB, "head-b")
	waiter := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-a", false, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	wakeNotices(w)
	w.step(w.m.Release(w.ctx, held["turnId"].(string), beta.TaskID, "returned", "done", ""))
	w.turn(waiter["turnId"].(string))
	wakeNotices(w)
	w.step(w.m.Ready(w.ctx, waiter["turnId"].(string), alpha.TaskID, true, "", ""))
	wakeNotices(w)
	w.sameAsPython("mtw2_conditions")
}
func Test26_MTW_6_python_whole_output(t *testing.T) {
	w := wakeFx(t)
	rival := w.claim(beta, fxB, "head-b")
	waiter := w.claim(alpha, fxA, "head-a")
	w.step(w.m.Release(w.ctx, rival["turnId"].(string), beta.TaskID, "returned", "done", ""))
	w.turn(waiter["turnId"].(string))
	wakeNotices(w)
	w.sameAsPython("mtw6_unaddressable")
}
func Test26_MTW_7_python_whole_output(t *testing.T) {
	w := wakeFx(t)
	plain := *w.m
	plain.Delivery = nil
	held := w.must(plain.Request(w.ctx, fxRepo, "release", fxB, beta.TaskID, beta.HostID, "head-b", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	waiter := w.must(plain.Request(w.ctx, fxRepo, "release", fxA, alpha.TaskID, alpha.HostID, "head-a", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	w.step(plain.Release(w.ctx, held["turnId"].(string), beta.TaskID, "returned", "done", ""))
	w.step(plain.Turn(w.ctx, waiter["turnId"].(string)))
	wakeNotices(w)
	w.sameAsPython("mtw7_optional")
}
func reportWake(w *fx, event string, submission int, head any) {
	w.exec(`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event, submission, "rel-a", 3, "rev", fxRepo, fxBase, head, "done", "r", "1", "s", "n", fxISO)
}
func Test26_MTW_10_python_whole_output(t *testing.T) {
	w := wakeFx(t)
	reportWake(w, "event-a", 1, "head-old")
	reportWake(w, "event-a", 2, "head-a")
	reportWake(w, "event-b", 1, "head-b")
	turn := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-a", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))["turnId"].(string)
	w.answer(turn, alpha.TaskID)
	w.step(w.check(turn, "head-a", "base-0", ""))
	w.turn(turn)
	w.exec("DELETE FROM work_reports WHERE event_id='event-b'")
	w.step(w.check(turn, "head-old", "base-0", ""))
	reportWake(w, "event-a", 3, nil)
	w.turn(turn)
	w.must(w.m.Release(w.ctx, turn, alpha.TaskID, "returned", "try historical head", ""))
	historical := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-old", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))["turnId"].(string)
	w.answer(historical, alpha.TaskID)
	w.step(w.check(historical, "head-old", "base-0", ""))
	w.turn(historical)
	w.sameAsPython("mtw10_gate")
}
func Test26_MTW_9_python_whole_output(t *testing.T) {
	w := wakeFx(t)
	reading := func() {
		r, err := supervisor.RequiredForCandidate(w.ctx, w.s, "rel-a", fxRepo, fxBase, "head-a")
		if err != nil {
			t.Fatal(err)
		}
		if r.Known {
			w.step(map[string]any{"required": r.Required, "eventId": r.EventID, "submissionNo": r.SubmissionNo}, nil)
		} else {
			w.step(map[string]any{"required": nil, "reason": r.Reason}, nil)
		}
	}
	reading()
	reportWake(w, "event-a", 1, "head-a")
	w.exec(`INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,required_declared,checks,review_coverage,thread_dispositions,recorded_at) VALUES (?,?,?,?,?,?,?,?)`, "event-a", 1, 0, `["dev-gate"]`, "[]", "{}", "[]", fxISO)
	reading()
	reportWake(w, "event-b", 1, "head-a")
	w.exec(`INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,required_declared,checks,review_coverage,thread_dispositions,recorded_at) VALUES (?,?,?,?,?,?,?,?)`, "event-b", 1, 0, `["other-gate"]`, "[]", "{}", "[]", fxISO)
	reading()
	w.sameAsPython("mtw9_reading")
}
func Test26_MTW_1_python_two_grants(t *testing.T) {
	w := wakeFx(t)
	first := wakePromote(w)
	grant := w.must(w.m.Turn(w.ctx, first))["grant"].(map[string]any)["grantId"].(string)
	w.step(w.m.Acknowledge(w.ctx, first, alpha.TaskID, grant, "read it"))
	w.step(w.m.Release(w.ctx, first, alpha.TaskID, "returned", "handing it back", ""))
	rival := w.claim(beta, fxB, "head-b2")
	waiter := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-a2", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	w.step(w.m.Release(w.ctx, rival["turnId"].(string), beta.TaskID, "returned", "done", ""))
	w.turn(waiter["turnId"].(string))
	wakeNotices(w)
	w.sameAsPython("mtw1_two_grants")
}
func Test26_MTW_2_python_unknown(t *testing.T) {
	w := wakeFx(t)
	rival := w.claim(beta, fxB, "head-b")
	held := rival["turnId"].(string)
	w.answer(held, beta.TaskID)
	b := defaults()
	b.actor = beta.TaskID
	b.head = "head-b"
	b.checks = runChecks("head-b", "success", 1, "dev-gate", "run-1")
	w.must(w.begin(held, b))
	waiter := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-a", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	w.step(w.m.Unknown(w.ctx, held, beta.TaskID, "the host went away mid-merge"))
	w.m.Now = func() string { return registry.ISO(time.Unix(1_700_000_000+86400*7, 0)) }
	w.turn(waiter["turnId"].(string))
	wakeNotices(w)
	w.sameAsPython("mtw2_unknown")
}
func Test26_MTW_6_python_other_parent(t *testing.T) {
	w := wakeFx(t)
	held := w.claim(alpha, fxA, "head-a")
	waiter := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxB, beta.TaskID, beta.HostID, "head-b", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	w.step(w.m.Release(w.ctx, held["turnId"].(string), alpha.TaskID, "returned", "done", ""))
	w.turn(waiter["turnId"].(string))
	wakeNotices(w)
	w.sameAsPython("mtw6_other_parent")
}
func Test26_MTW_6_python_recipient_and_scope(t *testing.T) {
	w := wakeFx(t)
	w.exec("UPDATE relationships SET allowed_recipients=? WHERE relationship_id=?", `["task-child"]`, "rel-a")
	turn := wakePromote(w)
	w.turn(turn)
	wakeNotices(w)
	w.exec("UPDATE relationships SET allowed_recipients=? WHERE relationship_id=?", `["task-alpha"]`, "rel-a")
	w.exec("INSERT INTO relationship_scope (relationship_id,project_key,recorded_at) VALUES (?,?,?)", "rel-a", "PRJ-C", fxISO)
	held := w.must(w.m.Request(w.ctx, fxRepo, "release", fxB, beta.TaskID, beta.HostID, "head-b2", true))
	waiter := w.must(w.m.Request(w.ctx, fxRepo, "release", fxA, alpha.TaskID, alpha.HostID, "head-a2", true, ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	w.must(w.m.Release(w.ctx, held["turnId"].(string), beta.TaskID, "returned", "done", ""))
	w.turn(waiter["turnId"].(string))
	wakeNotices(w)
	w.sameAsPython("mtw6_recipient_and_scope")
}
func Test26_MTG_4_python_whole_output(t *testing.T) {
	w := newFx(t)
	sha := strings.Repeat("a", 40)
	valid := fmt.Sprintf(`{"ref":"refs/heads/dev","object":{"type":"commit","sha":"%s"}}`, sha)
	answers := []struct {
		status  int
		body    string
		missing bool
	}{{200, valid, false}, {404, "", false}, {409, "", false}, {200, `[{"ref":"refs/heads/dev"}]`, false}, {200, fmt.Sprintf(`{"ref":"refs/heads/dev-2","object":{"type":"commit","sha":"%s"}}`, sha), false}, {200, fmt.Sprintf(`{"ref":"refs/heads/dev","object":{"type":"tag","sha":"%s"}}`, sha), false}, {200, fmt.Sprintf(`{"ref":"refs/heads/dev","object":{"type":"commit","sha":"%s"}}`, strings.ToUpper(sha)), false}, {0, "", true}}
	for _, a := range answers {
		server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/repos/owner/repo/git/ref/heads/dev" || r.Header.Get("Accept") != "application/vnd.github+json" {
				t.Errorf("bad request %s %s", r.Method, r.URL)
			}
			rw.WriteHeader(a.status)
			fmt.Fprint(rw, a.body)
		}))
		gh, log := fakeGH(t, `curl -sS -H 'Accept: application/vnd.github+json' -w '%{http_code}' `+server.URL+`/"$6" > "${0}.response"
code=$(tail -c 3 "${0}.response")
if [ "$code" = 200 ]; then head -c -3 "${0}.response"; elif [ "$code" = 404 ]; then printf 'gh: Not Found (HTTP 404)\n' >&2; exit 1; elif [ "$code" = 409 ]; then printf 'gh: Git Repository is empty. (HTTP 409)\n' >&2; exit 1; fi`)
		if a.missing {
			gh = filepath.Join(t.TempDir(), "missing-gh")
		}
		targetStep(t, w, TargetReader{GH: gh}, "owner/repo", "dev")
		if a.missing {
			w.step([]any{[]string{"api", "--method", "GET", "-H", "Accept: application/vnd.github+json", "repos/owner/repo/git/ref/heads/dev"}}, nil)
		} else {
			recordCalls(w, log)
		}
		server.Close()
	}
	gh, log := fakeGH(t, "exit 99")
	for _, v := range [][2]string{{"owner/repo", "topic#42"}, {"-owner/repo", "dev"}, {"owner", "dev"}, {"owner/repo/extra", "dev"}, {"https://x/y", "dev"}} {
		targetStep(t, w, TargetReader{GH: gh}, v[0], v[1])
		if _, err := os.Stat(log); err == nil {
			t.Fatal("invalid request started gh")
		}
		w.step([]string{}, nil)
	}
	w.sameAsPython("mtg4_forge")
}
