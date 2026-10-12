package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// seeded is a run with two phases: a completed executor on n1 that committed and was reviewed, an architect call in recovery_hold,
// and a question of n1.
func seeded(t *testing.T, w *world) *run {
	t.Helper()
	r := w.newRun("run-a", "thread-owner")
	r.phase("design")
	r.completed("c_arch", "architect", "", "thr-arch", 700)
	r.phase("build")
	r.completed("c_exec", "executor", "n1", "thr-exec", 1200)
	r.op("c_commit", "crw.commit", map[string]any{"node": "n1", "commit": "abcdef0123456789", "call": "c_exec"})
	r.op("c_review", "crw.review", map[string]any{"node": "n1", "head": "abcdef0123456789", "state": "verdict", "verdict": "needs_changes", "call": "c_rev"})
	r.hold("c_lost", "executor", "n2")
	r.close()
	return r
}

// TestShowTreeIncludesRecoveryHold: `crw flow show` is the tree phase, then agent; a call whose outcome is unknown is marked
// recovery_hold on the call, on its node and in the run's counts, with the command that settles it, and the commits, the review
// and the tokens of a node are on its line.
func TestShowTreeIncludesRecoveryHold(t *testing.T) {
	w := newWorld(t)
	seeded(t, w)
	td := w.deps()
	code, out, errOut := td.do("show", "run-a", "--state", w.state)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{
		"run run-a  project-lane  dead", "phase design", "architect", "thr-arch", "phase build", "executor       n1       completed", "thread thr-exec", "tokens 1200",
		"n2       outcome_unknown (dispatch_unknown)", "[recovery_hold]", "1 call(s) in recovery_hold: crw flow recover", "node n1  commits abcdef01234567", "review c_rev needs_changes verdict@abcdef01234567", "node n2  [recovery_hold]", "no process holds the run: crw flow resume run-a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the tree lacks %q:\n%s", want, out)
		}
	}
	// the order is the tree's: design before build, and the held call after the completed ones
	if strings.Index(out, "phase design") > strings.Index(out, "phase build") || strings.Index(out, "thr-exec") > strings.Index(out, "n2       outcome_unknown") {
		t.Errorf("the tree is out of order:\n%s", out)
	}
	code, out, _ = td.do("show", "run-a", "--state", w.state, "--json")
	var d RunDetail
	if code != 0 || json.Unmarshal([]byte(out), &d) != nil {
		t.Fatalf("show --json: %d %s", code, out)
	}
	if d.Holds != 1 || d.State != StateDead || len(d.Phases) != 2 || d.Tokens != 1900 {
		t.Errorf("the reading: holds %d state %s phases %d tokens %d", d.Holds, d.State, len(d.Phases), d.Tokens)
	}
	var held *AgentNode
	for i := range d.Phases[1].Agents {
		if d.Phases[1].Agents[i].CallID == "c_lost" {
			held = &d.Phases[1].Agents[i]
		}
	}
	if held == nil || !held.RecoveryHold || held.Node != "n2" {
		t.Errorf("the lost call in the build phase: %+v", d.Phases[1].Agents)
	}
	var n1, n2 NodeInfo
	for _, n := range d.Nodes {
		switch n.Node {
		case "n1":
			n1 = n
		case "n2":
			n2 = n
		}
	}
	if len(n1.Commits) != 1 || len(n1.Reviews) != 1 || n1.Reviews[0].Verdict != "needs_changes" || n1.Tokens != 1200 || len(n1.Threads) != 1 || n1.RecoveryHold || !n2.RecoveryHold {
		t.Errorf("the nodes: %+v / %+v", n1, n2)
	}
	if code, _, errOut := td.do("show", "run-none", "--state", w.state); code != 2 || !strings.Contains(errOut, "no such run") {
		t.Errorf("an unknown run: %d %q", code, errOut)
	}
	// a run a process holds is running, and ps lists it with the others that are not over
	lock, err := TakeLock(w.state, "run-a", "test", td.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	_, out, _ = td.do("ps", "--state", w.state)
	if !strings.Contains(out, "run-a  running  project-lane") {
		t.Errorf("ps:\n%s", out)
	}
	if _, _, errOut := td.do("pause", "run-a", "--state", w.state); errOut != "" {
		t.Errorf("pause: %q", errOut)
	}
}

// TestWatchFollowsJournalTail: watch reads the rows of a run as the relay's journal-tail reads its journal: the cursor is the seq
// of the last row, a read from the cursor gives the rows after it once and in order, a limit pages with `more`, and a wait returns
// as soon as a row arrives and answers an empty page with the cursor it was given when none does.
func TestWatchFollowsJournalTail(t *testing.T) {
	w := newWorld(t)
	r := seeded(t, w)
	old := TailPoll
	TailPoll = 10 * time.Millisecond
	defer func() { TailPoll = old }()
	rd, err := OpenReader(ctx, w.state, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()

	var all []Row
	cursor := 0
	for i := 0; ; i++ {
		page, err := rd.Tail(ctx, "run-a", cursor, 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		if page.After != cursor || len(page.Rows) > 4 || (page.More && len(page.Rows) != 4) {
			t.Fatalf("page %d: %+v", i, page)
		}
		for _, row := range page.Rows {
			if row.Seq <= cursor {
				t.Fatalf("row %d is not after the cursor %d", row.Seq, cursor)
			}
		}
		all = append(all, page.Rows...)
		cursor = page.Cursor
		if !page.More {
			break
		}
	}
	events, _, _ := r.j.Events(), 0, 0
	if len(all) != len(events) {
		t.Fatalf("the pages hold %d rows, the journal %d", len(all), len(events))
	}
	// at the end: an empty page keeps the cursor, and a wait with no row ends with the same
	page, err := rd.Tail(ctx, "run-a", cursor, 4, 30*time.Millisecond)
	if err != nil || len(page.Rows) != 0 || page.Cursor != cursor || page.More {
		t.Fatalf("after the last row: %+v %v", page, err)
	}
	// a row that arrives during the wait ends it
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(60 * time.Millisecond)
		r2, err := w.reopen("run-a")
		if err != nil {
			t.Error(err)
			return
		}
		r2.phase("milestone")
		r2.close()
	}()
	start := time.Now()
	page, err = rd.Tail(ctx, "run-a", cursor, 4, 5*time.Second)
	wg.Wait()
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Event != "phase" || page.Cursor != cursor+1 || time.Since(start) > 3*time.Second {
		t.Fatalf("the wait: %+v %v after %v", page, err, time.Since(start))
	}

	// the command: the rows after --after, then the cursor line to pass back
	td := w.deps()
	code, out, errOut := td.do("watch", "run-a", "--state", w.state, "--after", itoa(cursor-1), "--limit", "10")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.Contains(lines[0], "call/outcome_unknown") || !strings.Contains(lines[1], "flow/phase") || !strings.HasPrefix(lines[2], "cursor "+itoa(cursor+1)+"  more false") {
		t.Fatalf("watch: exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = td.do("watch", "run-a", "--state", w.state, "--after", itoa(cursor+1), "--json")
	var p Page
	if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &p) != nil || len(p.Rows) != 0 || p.Cursor != cursor+1 {
		t.Fatalf("watch --json at the end: %d %s", code, out)
	}
	for _, bad := range [][]string{{"--after", "-1"}, {"--limit", "0"}, {"--limit", "1001"}, {"--wait", "3601"}} {
		if code, _, _ := td.do(append([]string{"watch", "run-a", "--state", w.state}, bad...)...); code != 2 {
			t.Errorf("watch %v: exit %d", bad, code)
		}
	}
}

// reopen opens the journal of an existing run to append more rows, as a later process does.
func (w *world) reopen(id string) (*run, error) {
	j, err := openJournal(w, id)
	if err != nil {
		return nil, err
	}
	return &run{w: w, id: id, j: j}, nil
}

// TestStatusLineScopedToOwnerThread: the status line is the line of the runs a thread owns and are not over, printed by the Stop
// hook for that thread only: another thread, a thread that owns only finished runs and an unreadable payload get nothing.
func TestStatusLineScopedToOwnerThread(t *testing.T) {
	w := newWorld(t)
	seeded(t, w)
	b := w.newRun("run-b", "thread-owner")
	b.phase("build")
	b.completed("c_b1", "executor", "n9", "thr-9", 2500)
	b.close()
	done := w.newRun("run-done", "thread-other")
	done.completed("c_d", "executor", "n1", "thr-d", 10)
	done.finish(engine.RunCompleted, "")
	done.close()
	td := w.deps()

	code, out, _ := td.do("status-line", "--thread", "thread-owner", "--state", w.state)
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, "crw flow run-b dead · build · 1 calls") || !strings.Contains(out, "2.5k tokens") || !strings.Contains(out, "(+1 more)") {
		t.Errorf("the owner's line: exit %d %q", code, out)
	}
	for _, thread := range []string{"thread-other", "thread-nobody", ""} {
		if _, out, _ := td.do("status-line", "--thread", thread, "--state", w.state); out != "" {
			t.Errorf("thread %q is shown %q", thread, out)
		}
	}
	// the Stop hook answers with a system message for the owner and with nothing for any other thread
	hook := func(payload string) string {
		var out bytes.Buffer
		if code := RunStopHook(ctx, strings.NewReader(payload), &out, td.Deps, w.state, "", 5*time.Second); code != 0 {
			t.Errorf("the hook exits %d", code)
		}
		return out.String()
	}
	if got := hook(`{"session_id":"thread-owner","turn_id":"t1","hook_event_name":"Stop"}`); !strings.Contains(got, `"systemMessage":"crw flow run-b dead`) {
		t.Errorf("the owner's Stop: %q", got)
	}
	for _, payload := range []string{`{"session_id":"thread-other"}`, `{"session_id":"thread-nobody"}`, `{}`, `not json`, ``} {
		if got := hook(payload); got != "" {
			t.Errorf("the Stop of %q printed %q", payload, got)
		}
	}
	// a store that cannot be read prints nothing and fails nothing
	var quiet bytes.Buffer
	if code := RunStopHook(ctx, strings.NewReader(`{"session_id":"thread-owner"}`), &quiet, &Deps{Reader: OpenReader}, t.TempDir(), "", time.Second); code != 0 || quiet.Len() != 0 {
		t.Errorf("no store: %d %q", code, quiet.String())
	}
}
