package recall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// CRW-1089 (A8-10): what the hook shows is what the history counts, once, after it was shown.

func accountingSessions(n int) []CwdSession {
	out := []CwdSession{}
	for i := 0; i < n; i++ {
		id := fmt.Sprint("t", i)
		out = append(out, CwdSession{Path: id + ".jsonl", ThreadID: &id, Date: "2026-09-09", Excerpt: strings.Repeat("x", 100)})
	}
	return out
}

func accountingDeps(calls *hookContextCalls, sessions []CwdSession) RecallContextDeps {
	store := &hookContextTestStore{calls: calls, counts: map[string]float64{}}
	return RecallContextDeps{Invocation: "crw",
		ListCwdSessions: func(_ string, n int) ([]CwdSession, error) { return sessions[:min(n, len(sessions))], nil },
		OpenHitCounts:   func() (HitCountStore, error) { calls.Opened++; return store, nil }}
}

func TestHookContextCountsOnlyTheRenderedEntries(t *testing.T) {
	calls := &hookContextCalls{}
	r := BuildCwdContextResult("/repo", accountingDeps(calls, accountingSessions(8)), RecallBudget{Chars: 800, TopN: 5, Snippet: 100})
	shown := strings.Count(r.Text, "  •")
	if r.Outcome != CwdContextHits || shown != 2 {
		t.Fatalf("fixture: %d entries shown, %s", shown, r.Outcome)
	}
	if !reflect.DeepEqual(r.Refs, []string{"thread:t0", "thread:t1"}) {
		t.Fatalf("refs %v are not the shown entries", r.Refs)
	}
	if len(calls.Bump) != 0 || len(calls.Read) != 1 || calls.Closed != 1 {
		t.Fatalf("choosing counted or kept the store: %+v", calls)
	}
}

func TestHookContextSummaryFailureAndEmptyBudgetCountNothing(t *testing.T) {
	calls := &hookContextCalls{}
	deps := accountingDeps(calls, accountingSessions(8))
	deps.LoadSummaryIndex = func() (map[string]SummaryEntry, error) { return nil, errors.New("summary failed") }
	if r := BuildCwdContextResult("/repo", deps, FullBudget()); r.Outcome != CwdContextUnavailable || len(r.Refs) != 0 {
		t.Fatalf("%+v", r)
	}
	deps = accountingDeps(calls, accountingSessions(3))
	r := BuildCwdContextResult("/repo", deps, RecallBudget{Chars: 50, TopN: 3, Snippet: 100})
	if r.Outcome != CwdContextEmpty || r.Text != "" || len(r.Refs) != 0 {
		t.Fatalf("a budget that fits no entry must give an empty result: %+v", r)
	}
	if len(calls.Bump) != 0 {
		t.Fatal("counted", calls.Bump)
	}
}

func TestHookContextEmptyThreadIDIsAbsent(t *testing.T) {
	empty := ""
	hit := func(file, ts string, id *string) ChatHit {
		return ChatHit{TS: ts, Text: file, ThreadID: id, File: file, Cwd: strPtrAccounting("/repo")}
	}
	var gotRefs []string
	deps := RecallContextDeps{Invocation: "crw", SearchChat: func(string, ChatSearchOptions) (ChatSearchResult, error) {
		return ChatSearchResult{Hits: []ChatHit{
			hit("/a.jsonl", "2026-09-09T00:00:00.000Z", &empty), hit("/b.jsonl", "2026-09-08T00:00:00.000Z", &empty),
			hit("/b.jsonl", "2026-09-07T00:00:00.000Z", nil), // the same file again, without an id: one entry
		}}, nil
	}}
	r := BuildCwdContextResult("/repo", deps, FullBudget())
	gotRefs = r.Refs
	if !reflect.DeepEqual(gotRefs, []string{"file:/a.jsonl", "file:/b.jsonl"}) {
		t.Fatalf("refs %v: an empty thread id is no thread id, and files are told apart by the history's own ref", gotRefs)
	}
}

func strPtrAccounting(s string) *string { return &s }

func TestHookContextDatesAreParsedBeforeTheyAreCompared(t *testing.T) {
	calls := &hookContextCalls{}
	sessions := accountingSessions(3)
	sessions[0].Date = "2026-13-45" // not a date
	sessions[1].Date = "2026-09-09"
	sessions[2].Date = "2026-9-10" // not the form
	r := BuildCwdContextResult("/repo", accountingDeps(calls, sessions), FullBudget())
	if !strings.Contains(r.Text, "as of 2026-09-09,") || !strings.Contains(r.Text, "[undated]") || strings.Contains(r.Text, "2026-13-45") {
		t.Fatalf("an impossible date was shown or won: %q", r.Text)
	}
	for i := range sessions {
		sessions[i].Date = "never"
	}
	r = BuildCwdContextResult("/repo", accountingDeps(calls, sessions), FullBudget())
	if !strings.Contains(r.Text, "as of an unknown date,") {
		t.Fatalf("a block without a date keeps its staleness notice: %q", r.Text)
	}
	// Rollout timestamps carry the date in their first ten characters; the label is normalized.
	chat := RecallContextDeps{Invocation: "crw", SearchChat: func(string, ChatSearchOptions) (ChatSearchResult, error) {
		return ChatSearchResult{Hits: []ChatHit{{TS: "2026-09-04T12:00:00.000Z", Text: "a", File: "/a", Cwd: strPtrAccounting("/repo")}, {TS: "2026-02-30T00:00:00.000Z", Text: "b", File: "/b", Cwd: strPtrAccounting("/repo")}}}, nil
	}}
	if r = BuildCwdContextResult("/repo", chat, FullBudget()); !strings.Contains(r.Text, "as of 2026-09-04,") || !strings.Contains(r.Text, "[undated]") {
		t.Fatal(r.Text)
	}
}

func TestHookContextClipAndCountsAreBounded(t *testing.T) {
	long := hookContextUnits(strings.Repeat("x", 50), nil)
	for n, want := range map[int]string{-5: "", 0: "", 1: ".", 2: "..", 3: "...", 4: "x...", 50: strings.Repeat("x", 50), 60: strings.Repeat("x", 50)} {
		if got := string(hookContextToRunes(hookContextClip(long, n))); got != want {
			t.Errorf("clip to %d: %q, want %q", n, got, want)
		}
	}
	calls := &hookContextCalls{}
	for _, snippet := range []int{0, 1, 2} {
		r := BuildCwdContextResult("/repo", accountingDeps(calls, accountingSessions(1)), RecallBudget{Chars: 1400, TopN: 5, Snippet: snippet})
		line := strings.SplitN(strings.SplitN(r.Text, "  • [2026-09-09] ", 2)[1], "\n", 2)[0]
		if len(line) != snippet+2 { // the quotes
			t.Errorf("snippet budget %d gave %s", snippet, line)
		}
	}
	// A negative count asks for nothing; it neither slices from the end nor fails.
	for _, b := range []RecallBudget{{-1, 5, 100}, {1400, -1, 100}, {1400, 5, -100}} {
		r := BuildCwdContextResult("/repo", accountingDeps(calls, accountingSessions(3)), b)
		if b.Chars < 0 || b.TopN < 0 {
			if r.Outcome != CwdContextEmpty {
				t.Errorf("%+v: %+v", b, r)
			}
		} else if !strings.Contains(r.Text, `""`) {
			t.Errorf("%+v: %q", b, r.Text)
		}
	}
}

func hookContextToRunes(u []uint16) []rune {
	out := []rune{}
	for _, c := range u {
		out = append(out, rune(c))
	}
	return out
}

// accountingHook runs the session-start hook against a sidecar index holding n sessions of /repo.
type accountingHook struct {
	t    *testing.T
	env  host.LookupEnv
	path string
	db   *RwDb
	refs []string
}

func newAccountingHook(t *testing.T, n int) *accountingHook {
	t.Helper()
	home, crw := t.TempDir(), t.TempDir()
	path := filepath.Join(crw, "recall", "index.sqlite")
	db, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &accountingHook{t: t, path: path, db: db}
	for i := 0; i < n; i++ {
		id := fmt.Sprint("t", i)
		recallSQL(t, db, fmt.Sprintf(`INSERT INTO files(path,mtime_ms,size,thread_id,cwd,source,date) VALUES('%s.jsonl',0,0,'%s','/repo','main','2026-09-0%d')`, id, id, 9-i))
		recallSQL(t, db, fmt.Sprintf(`INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES('%s.jsonl',0,'2026-09-09','user','content',0,'opening message of session %d')`, id, i))
		h.refs = append(h.refs, "thread:"+id)
	}
	env := map[string]string{"HOME": home, "CODEX_HOME": filepath.Join(home, ".codex"), "CRW_HOME": crw, "CRW_BIN": "crw"}
	h.env = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	return h
}

func (h *accountingHook) run(out interface{ Write([]byte) (int, error) }) {
	h.t.Helper()
	notDedicated := false
	start := func(home, path, cwd, source string, deps RecallContextDeps) string {
		return HandleSessionStart("", cwd, source, SessionStartOptions{Home: home, DedicatedTools: &notDedicated}, deps)
	}
	code := recallHookRun(context.Background(), "session-start", strings.NewReader(`{"hook_event_name":"SessionStart","cwd":"/repo","session_id":"s","source":"startup"}`), out, h.env, "/repo", start)
	if code != 0 {
		h.t.Fatal("hook exit", code)
	}
}

func (h *accountingHook) counts() map[string]float64 { return readHitCounts(h.db, h.refs) }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestSessionStartCountsTheEntriesItWrote(t *testing.T) {
	h := newAccountingHook(t, 3)
	var out bytes.Buffer
	h.run(&out)
	if strings.Count(out.String(), "opening message of session") != 3 {
		t.Fatal("the answer does not carry the three sessions:", out.String())
	}
	if want := map[string]float64{"thread:t0": 1, "thread:t1": 1, "thread:t2": 1}; !reflect.DeepEqual(h.counts(), want) {
		t.Fatalf("counts %v, want %v", h.counts(), want)
	}
	if rows := indexRows(t, h.db, "SELECT event FROM recall_hit_events"); len(rows) != 0 {
		t.Fatal("a counted event is kept after nothing can retry it", rows)
	}
}

func TestSessionStartCountsNothingWhenTheAnswerWasNotWritten(t *testing.T) {
	h := newAccountingHook(t, 3)
	h.run(failingWriter{})
	if len(h.counts()) != 0 {
		t.Fatal("entries nobody received were counted", h.counts())
	}
}

// A history that cannot be written does not hide the context, and a failure part way counts none.
func TestSessionStartHistoryFailureKeepsTheContextAndCountsNone(t *testing.T) {
	h := newAccountingHook(t, 3)
	recallSQL(t, h.db, "CREATE TRIGGER refuse BEFORE INSERT ON recall_hit_counts WHEN NEW.ref='thread:t1' BEGIN SELECT RAISE(ABORT,'fault'); END")
	var out bytes.Buffer
	h.run(&out)
	if strings.Count(out.String(), "opening message of session") != 3 {
		t.Fatal("the failed history write hid the context:", out.String())
	}
	if len(h.counts()) != 0 {
		t.Fatal("part of a failed count stayed:", h.counts())
	}
	if rows := indexRows(t, h.db, "SELECT name FROM sqlite_master WHERE name='recall_hit_events'"); len(rows) != 0 {
		t.Fatal("a failed count left its event table behind", rows) // The table is created inside the transaction.
	}
	recallSQL(t, h.db, "DROP TRIGGER refuse")
	h.run(&out)
	if want := map[string]float64{"thread:t0": 1, "thread:t1": 1, "thread:t2": 1}; !reflect.DeepEqual(h.counts(), want) {
		t.Fatalf("the next start counts as usual: %v", h.counts())
	}
}

// Counting an event again, as a retry of an attempt with an unknown outcome does, changes nothing.
func TestHitEventCountsOnce(t *testing.T) {
	db, _ := indexTestDB(t)
	refs := []string{"thread:a", "thread:b"}
	for range 3 {
		if err := recordHitEvent(db, "event-1", refs, "2026-09-09T00:00:00.000Z"); err != nil {
			t.Fatal(err)
		}
	}
	if got := readHitCounts(db, refs); !reflect.DeepEqual(got, map[string]float64{"thread:a": 1, "thread:b": 1}) {
		t.Fatal(got)
	}
	if err := recordHitEvent(db, "event-2", refs, "2026-09-09T00:00:01.000Z"); err != nil {
		t.Fatal(err)
	}
	if got := readHitCounts(db, refs); got["thread:a"] != 2 {
		t.Fatal(got)
	}
}

// The store the hook uses keeps that promise too: a second Bump with a counted event changes nothing.
func TestSidecarStoreBumpCountsAnEventOnce(t *testing.T) {
	db, _ := indexTestDB(t)
	store := &hookContextSidecarStore{db: db}
	for range 2 {
		if err := store.Bump("same-event", []string{"thread:a"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := readHitCounts(db, []string{"thread:a"})["thread:a"]; got != 1 {
		t.Fatalf("one event counted %v times", got)
	}
	if err := store.Bump("next-event", []string{"thread:a"}); err != nil {
		t.Fatal(err)
	}
	if got := readHitCounts(db, []string{"thread:a"})["thread:a"]; got != 2 {
		t.Fatalf("a new event counts: %v", got)
	}
}

func TestHitEventsAreBounded(t *testing.T) {
	db, _ := indexTestDB(t)
	for i := range hitEventKeep + 50 {
		if err := recordHitEvent(db, fmt.Sprint("e", i), []string{"thread:a"}, "2026-09-09T00:00:00.000Z"); err != nil {
			t.Fatal(err)
		}
	}
	if n := hitCountNumber(indexRows(t, db, "SELECT COUNT(*) AS n FROM recall_hit_events")[0]["n"]); n != hitEventKeep {
		t.Fatalf("%v events kept", n)
	}
	// Events older than the window go; the history they counted stays.
	if err := recordHitEvent(db, "late", []string{"thread:a"}, "2026-09-11T00:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	if n := hitCountNumber(indexRows(t, db, "SELECT COUNT(*) AS n FROM recall_hit_events")[0]["n"]); n != 1 {
		t.Fatalf("%v events kept after the window", n)
	}
	if got := readHitCounts(db, []string{"thread:a"}); got["thread:a"] != hitEventKeep+51 {
		t.Fatal(got)
	}
}
