package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
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

// TestWatchEndsOnAPausedRunWithNoProcess (CRW-1487): a follow of a run that ended paused, with no process to write more, ends as it
// does on a dead or a held run instead of waiting for rows nobody writes.
func TestWatchEndsOnAPausedRunWithNoProcess(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-wp", "thread-owner")
	r.endPaused()
	r.close()
	td := w.deps()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	w.close()
	code := RunWith(cctx, td.Deps, []string{"watch", "run-wp", "--follow", "--wait", "1", "--state", w.state}, strings.NewReader(""), &out, &errOut)
	w.open()
	if cctx.Err() != nil {
		t.Fatalf("the follow of a paused end with no process did not end: exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if code != 0 || !strings.Contains(out.String(), "state paused") {
		t.Errorf("the follow: exit %d\n%s%s", code, out.String(), errOut.String())
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

	// the command: without --after the tree as of a cursor, recovery_hold included; with it the rows after that cursor
	td := w.deps()
	code, out, errOut := td.do("watch", "run-a", "--state", w.state)
	treeCursor := itoa(cursor + 1)
	if code != 0 || !strings.Contains(out, "phase design") || !strings.Contains(out, "[recovery_hold]") || !strings.Contains(out, "phase milestone") || !strings.HasSuffix(strings.TrimSpace(out), "cursor "+treeCursor+"  state dead") {
		t.Fatalf("watch without a cursor: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = td.do("watch", "run-a", "--state", w.state, "--after", itoa(cursor-1), "--limit", "10")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.Contains(lines[0], "call/outcome_unknown") || !strings.Contains(lines[1], "flow/phase") || !strings.HasPrefix(lines[2], "cursor "+treeCursor+"  more false") {
		t.Fatalf("watch: exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = td.do("watch", "run-a", "--state", w.state, "--after", treeCursor, "--json")
	var p Page
	if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &p) != nil || len(p.Rows) != 0 || p.Cursor != cursor+1 {
		t.Fatalf("watch --json at the end: %d %s", code, out)
	}
	code, out, _ = td.do("watch", "run-a", "--state", w.state, "--json")
	var first struct {
		Cursor int       `json:"cursor"`
		Tree   RunDetail `json:"tree"`
	}
	if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &first) != nil || first.Cursor != cursor+1 || len(first.Tree.Phases) != 3 || first.Tree.Holds != 1 {
		t.Fatalf("watch --json without a cursor: %d %s", code, out)
	}
	// --follow prints the tree, then the rows after it as they arrive, and ends with the tree when the run has no process
	go func() {
		time.Sleep(150 * time.Millisecond)
		r3, err := w.reopen("run-a")
		if err != nil {
			t.Error(err)
			return
		}
		r3.phase("report")
		r3.close()
	}()
	code, out, errOut = td.doKeep("watch", "run-a", "--state", w.state, "--follow", "--wait", "1")
	if code != 0 || strings.Count(out, "phase design") != 2 || !strings.Contains(out, "flow/phase  title=report") || !strings.Contains(out, "phase report") {
		t.Fatalf("watch --follow: exit %d\n%s%s", code, out, errOut)
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

// A command that writes into a run (CRW-1204: crw flow report-up, report-ack, report-delivered) numbers its rows from
// engine.ForeignSeqBase, while the run, alive, goes on numbering its own from where it was. The cursor of watch and of the GUI's
// event stream is the seq of the run's own rows: it never jumps to a command's number, so the rows the run writes after a
// command wrote into it are still read, and the tree's cursor (LastSeq) is the run's own last row.
func TestWatchCursorStaysOnTheRunsOwnRowsWhenACommandWroteIntoIt(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-f", "thread-owner")
	r.phase("one")
	if _, err := (report.ForeignAppender{Store: w.st, Run: "run-f"}).Append(ctx, report.NewRow{Kind: report.KindReport, Subject: "blocked", Payload: map[string]any{"report_kind": report.KindProjectBlocked}}); err != nil {
		t.Fatal(err)
	}
	r.phase("two")
	rd, err := OpenReader(ctx, w.state, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	d, err := rd.Detail(ctx, "run-f")
	if err != nil {
		t.Fatal(err)
	}
	own := r.j.Events()
	last := 0
	for _, e := range own {
		if int64(e.Seq) < engine.ForeignSeqBase {
			last = max(last, e.Seq)
		}
	}
	if d.LastSeq != last {
		t.Errorf("the tree's cursor is %d, the run's own last row %d", d.LastSeq, last)
	}
	// following from 0 reads every row the run wrote, and the cursor stays below the commands' numbers
	cursor := 0
	var seen []string
	for {
		page, err := rd.Tail(ctx, "run-f", cursor, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Rows {
			seen = append(seen, row.Event)
		}
		cursor = page.Cursor
		if !page.More {
			break
		}
	}
	if int64(cursor) >= engine.ForeignSeqBase || cursor != last {
		t.Fatalf("the cursor after the last page is %d (the run's own last row %d)", cursor, last)
	}
	// a row the run writes after the command's is read from that cursor and from the tree's
	r.phase("three")
	for _, from := range []int{cursor, d.LastSeq} {
		page, err := rd.Tail(ctx, "run-f", from, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) == 0 || page.Rows[len(page.Rows)-1].Data["title"] != "three" {
			t.Errorf("from the cursor %d the page is %+v: the run's own row after the command's is lost", from, page.Rows)
		}
	}
	if strings.Join(seen, ",") != "started,phase,phase" {
		t.Errorf("the pages hold %v", seen)
	}
}

// CRW-1324 item 30(b): crw flow show names the execution policy file the run recorded when it started and the digest of
// its bytes, so an operator sees from outside which policy the run is bound to. A resumed run that read another digest
// records the change; show gives the policy the run holds now and the one it held before.
func TestShowNamesTheRecordedPolicyPathAndDigest(t *testing.T) {
	const (
		path   = "/home/op/.codex/execution-policy.json"
		first  = "1111111111111111111111111111111111111111111111111111111111111111"
		second = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	w := newWorld(t)
	r := w.newRun("run-pol", "thread-owner")
	r.j.Append(engine.Event{RunID: "run-pol", Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "policy_source": path, "policy_digest": first}})
	td := w.deps() // doKeep: the test goes on appending to the run's journal, so the world's store stays open
	code, out, errOut := td.doKeep("show", "run-pol", "--state", w.state)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if want := "policy " + path + "  digest " + first; !strings.Contains(out, want) {
		t.Errorf("show lacks %q:\n%s", want, out)
	}
	code, out, _ = td.doKeep("show", "run-pol", "--state", w.state, "--json")
	var d map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &d) != nil {
		t.Fatalf("show --json: %d %s", code, out)
	}
	if d["policy_path"] != path || d["policy_digest"] != first {
		t.Errorf("show --json policy = %v %v, want %q %q", d["policy_path"], d["policy_digest"], path, first)
	}

	// A resume that read another digest journals the change; show gives the digest the run holds now and the earlier one.
	r.j.Append(engine.Event{RunID: "run-pol", Kind: engine.KindRun, Event: engine.EvPolicyRepinned, Data: map[string]any{"old": first, "new": second, "old_source": path, "new_source": path}})
	code, out, errOut = td.doKeep("show", "run-pol", "--state", w.state)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if want := "policy " + path + "  digest " + second; !strings.Contains(out, want) || !strings.Contains(out, "repinned from "+first) {
		t.Errorf("show after a repin lacks the new digest and the old one:\n%s", out)
	}
}

// A run on the placeholder table (no policy file) says so rather than printing an empty path.
func TestShowSaysARunHasNoPolicyFile(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-fake", "thread-owner")
	r.j.Append(engine.Event{RunID: "run-fake", Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "policy_source": "", "policy_digest": ""}})
	code, out, errOut := w.deps().do("show", "run-fake", "--state", w.state)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "policy none (placeholder table, no execution policy file)") {
		t.Errorf("show does not say the run had no policy file:\n%s", out)
	}
}

// CRW-1360 d2: a start that reads the digest the run already holds repins nothing, so show stops
// printing the earlier "repinned from"; a real repin after that start prints it again. The journal is
// only read.
func TestShowClearsRepinnedFromOnALaterUnchangedStart(t *testing.T) {
	const (
		path   = "/home/op/.codex/execution-policy.json"
		first  = "1111111111111111111111111111111111111111111111111111111111111111"
		second = "2222222222222222222222222222222222222222222222222222222222222222"
		third  = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	w := newWorld(t)
	r := w.newRun("run-clear", "thread-owner")
	started := func(digest string) {
		r.j.Append(engine.Event{RunID: "run-clear", Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "policy_source": path, "policy_digest": digest}})
	}
	repinned := func(old, new string) {
		r.j.Append(engine.Event{RunID: "run-clear", Kind: engine.KindRun, Event: engine.EvPolicyRepinned, Data: map[string]any{"old": old, "new": new, "old_source": path, "new_source": path}})
	}
	td := w.deps()
	show := func() (string, map[string]any) {
		t.Helper()
		code, out, errOut := td.doKeep("show", "run-clear", "--state", w.state)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, errOut)
		}
		code, js, _ := td.doKeep("show", "run-clear", "--state", w.state, "--json")
		var d map[string]any
		if code != 0 || json.Unmarshal([]byte(js), &d) != nil {
			t.Fatalf("show --json: %d %s", code, js)
		}
		return out, d
	}
	started(first)
	started(second)
	repinned(first, second)
	if out, d := show(); !strings.Contains(out, "repinned from "+first) || d["policy_repinned_from"] != first {
		t.Fatalf("a repin right after its start is not shown:\n%s\n%v", out, d)
	}
	started(second) // the next start reads the same digest: nothing repinned
	if out, d := show(); strings.Contains(out, "repinned from") || d["policy_repinned_from"] != nil || !strings.Contains(out, "policy "+path+"  digest "+second) {
		t.Errorf("a later unchanged start still shows the old repin:\n%s\n%v", out, d)
	}
	started(second)
	repinned(second, third)
	if out, d := show(); !strings.Contains(out, "policy "+path+"  digest "+third+"  repinned from "+second) || d["policy_repinned_from"] != second {
		t.Errorf("a real repin after the start is not shown:\n%s\n%v", out, d)
	}
}

// CRW-1360 d2: a started row from before policy_source was journaled says the policy is not recorded
// (with its digest when it has one); only a row that records an empty policy_source is the placeholder
// table.
func TestShowDoesNotCallAnOldStartedRowAPlaceholderTable(t *testing.T) {
	const digest = "4444444444444444444444444444444444444444444444444444444444444444"
	w := newWorld(t)
	td := w.deps()
	for name, tc := range map[string]struct {
		data map[string]any
		want string
	}{
		"digest only": {map[string]any{"workflow": "project-lane", "policy_digest": digest}, "policy file not recorded  digest " + digest},
		"neither":     {map[string]any{"workflow": "project-lane"}, "policy not recorded"},
		"placeholder": {map[string]any{"workflow": "project-lane", "policy_source": "", "policy_digest": ""}, "policy none (placeholder table, no execution policy file)"},
	} {
		id := "run-old-" + strings.ReplaceAll(name, " ", "-")
		r := w.newRun(id, "thread-owner")
		r.j.Append(engine.Event{RunID: id, Kind: engine.KindRun, Event: "started", Data: tc.data})
		code, out, errOut := td.doKeep("show", id, "--state", w.state)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", name, code, errOut)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: show lacks %q:\n%s", name, tc.want, out)
		}
		if name != "placeholder" && strings.Contains(out, "placeholder table") {
			t.Errorf("%s: an old row is called a placeholder table:\n%s", name, out)
		}
	}
}

// TestTokensShowCachedFreshAndOutput (CRW-1327): `crw flow show`, `crw flow ps` and the run's digest split the tokens into cached input,
// fresh input (the input minus the cached) and output, with the reasoning output beside it: an architect call of 4,218,262 tokens
// whose input of 4,171,534 was 4,006,518 cached shows 165,016 fresh. A row written before the split (no cached input) reads it as 0.
func TestTokensShowCachedFreshAndOutput(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-tok", "thread-owner")
	r.phase("design")
	r.call("c_arch", "architect", "", 1, []string{engine.EvPrepared, engine.EvCompleted}, map[string]map[string]any{
		engine.EvCompleted: {"tokens": map[string]any{"input": 4171534, "cached_input": 4006518, "output": 46728, "reasoning_output": 31200, "total": 4218262}}})
	r.call("c_old", "reviewer", "", 1, []string{engine.EvPrepared, engine.EvCompleted}, map[string]map[string]any{
		engine.EvCompleted: {"tokens": map[string]any{"input": 80, "output": 20, "total": 100}}})
	r.close()
	td := w.deps()
	code, out, errOut := td.do("show", "run-tok", "--state", w.state)
	if code != 0 {
		t.Fatalf("show: exit %d %s", code, errOut)
	}
	for _, want := range []string{
		"tokens 4218362 (cached input 4006518, fresh input 165096, output 46748, reasoning 31200)",
		"tokens 4218262 (cached input 4006518, fresh input 165016, output 46728, reasoning 31200)",
		"tokens 100 (cached input 0, fresh input 80, output 20, reasoning 0)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = td.do("show", "run-tok", "--state", w.state, "--json")
	var d RunDetail
	if code != 0 || json.Unmarshal([]byte(out), &d) != nil {
		t.Fatalf("show --json: %d %s", code, out)
	}
	if want := (TokenSplit{CachedInput: 4006518, FreshInput: 165096, Output: 46748, ReasoningOutput: 31200}); d.Tokens != 4218362 || d.TokenSplit != want {
		t.Errorf("show --json: tokens %d split %+v", d.Tokens, d.TokenSplit)
	}
	if a := d.Phases[0].Agents[0]; a.TokenSplit != (TokenSplit{CachedInput: 4006518, FreshInput: 165016, Output: 46728, ReasoningOutput: 31200}) {
		t.Errorf("the architect call: %+v", a)
	}
	code, out, _ = td.do("ps", "--all", "--state", w.state)
	if code != 0 || !strings.Contains(out, "CACHED") || !strings.Contains(out, "FRESH") || !strings.Contains(out, "OUTPUT") || !regexp.MustCompile(`4218362\s+4006518\s+165096\s+46748`).MatchString(out) {
		t.Errorf("ps:\n%s", out)
	}
	events, _, err := loadRun(w, "run-tok")
	if err != nil {
		t.Fatal(err)
	}
	dg := BuildDigest(DigestInput{Run: RunRecord{RunID: "run-tok"}, Events: events, Spec: Spec{TokenBudget: 5000000}})
	var budget string
	for _, p := range dg.Providers {
		if p.Name == "token budget" {
			budget = p.Detail
		}
	}
	if budget != "4218362 of 5000000 tokens spent (cached input 4006518, fresh input 165096, output 46748, reasoning 31200)" {
		t.Errorf("the digest's token budget: %q", budget)
	}
}
