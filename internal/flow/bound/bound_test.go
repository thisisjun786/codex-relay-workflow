package bound

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	threadA = "0190a000-0000-7000-8000-00000000000a"
	callA   = "c_000000000001"
)

func engineLine(thread string) Line {
	one := 1
	return Line{RunID: "run-1", ThreadID: thread, Kind: KindEngine, CallID: callA, Role: "executor", NativeAllowance: &one,
		HelperPairs: map[string]Pair{HelperExplorer: {Model: "m", Effort: "high"}}, PolicyDigest: "sha256:x"}
}

// home points HOME at a temporary directory and returns the flow-bound directory under it.
func home(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg"))
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDirFollowsTheHomeAndNotXDG(t *testing.T) {
	dir := home(t)
	if !strings.HasSuffix(dir, filepath.Join(".local", "state", "crw", "flow-bound")) || !strings.HasPrefix(dir, os.Getenv("HOME")) {
		t.Fatalf("%s", dir)
	}
	t.Setenv("HOME", "")
	if _, err := Dir(); err == nil {
		t.Fatal("no home, no directory")
	}
}

func TestThreadAndCallIDForms(t *testing.T) {
	for id, want := range map[string]bool{
		threadA: true, "abc": true, "0": true, "": false, "-": false, "-abc": false, "ABC": false, "thr-fake-001": false, "a/b": false, "a.b": false,
		"..": false, strings.Repeat("a", 65): false, strings.Repeat("a", 64): true,
	} {
		if got := ValidThreadID(id); got != want {
			t.Errorf("ValidThreadID(%q) = %v", id, got)
		}
	}
	for id, want := range map[string]bool{callA: true, "": false, "../x": false, "a b": false, "a.answer": false, "-x": false} {
		if got := ValidCallID(id); got != want {
			t.Errorf("ValidCallID(%q) = %v", id, got)
		}
	}
}

func TestWriterMakesAPrivateDirectoryAndFileAndSyncsBeforeItReturns(t *testing.T) {
	dir := home(t)
	synced := 0
	w := &Writer{Sync: func(f *os.File) error { synced++; return f.Sync() }}
	for i := range 3 {
		l := engineLine(threadA)
		l.CallID = fmt.Sprintf("c_00000000000%d", i)
		if err := w.Append(l); err != nil {
			t.Fatal(err)
		}
	}
	if synced != 3 {
		t.Fatalf("%d syncs for 3 lines", synced)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory %v %v", info, err)
	}
	file := filepath.Join(dir, threadA+FileSuffix)
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file %v %v", info, err)
	}
	res := Lookup(threadA)
	if res.State != Engine || res.Line.CallID != "c_000000000002" || res.Line.Allowance() != 1 || res.Line.HelperPairs[HelperExplorer].Model != "m" {
		t.Fatalf("lookup %+v", res)
	}
	raw, _ := os.ReadFile(file)
	if n := strings.Count(string(raw), "\n"); n != 3 {
		t.Fatalf("%d lines: %q", n, raw)
	}
}

func TestWriterRefusesWhatItCannotTrust(t *testing.T) {
	dir := home(t)
	w := &Writer{}
	if err := w.Append(engineLine("thr-fake-001")); err == nil {
		t.Fatal("a thread id of another shape")
	}
	bad := engineLine(threadA)
	bad.HelperPairs = nil
	if err := w.Append(bad); err == nil {
		t.Fatal("a line without a helper pair")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a refused line made the directory")
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(engineLine(threadA)); err == nil || !strings.Contains(err.Error(), ReasonUnsafeDir) {
		t.Fatalf("a directory that others can read: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, threadA+FileSuffix)
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(engineLine(threadA)); err == nil || !strings.Contains(err.Error(), ReasonUnsafeFile) {
		t.Fatalf("a file that others can read: %v", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), file); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(engineLine(threadA)); err == nil {
		t.Fatal("a symlink in place of the file")
	}
}

// A line cut short by a crash stays a line of its own: the next line starts a new one, so it is the last line.
func TestWriterStartsANewLineAfterACutOne(t *testing.T) {
	dir := home(t)
	w := &Writer{}
	if err := w.Append(engineLine(threadA)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, threadA+FileSuffix)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"run_id":"cut`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if res := Lookup(threadA); res.State != Unavailable {
		t.Fatalf("a cut last line: %v", res.State)
	}
	if err := w.Append(engineLine(threadA)); err != nil {
		t.Fatal(err)
	}
	if res := Lookup(threadA); res.State != Engine {
		t.Fatalf("after the next line: %+v", res)
	}
}

func TestFlowBoundUnregisteredThreadStandaloneLookupStates(t *testing.T) {
	dir := home(t)
	if res := Lookup(threadA); res.State != Standalone {
		t.Fatalf("no directory: %v", res.State)
	}
	w := &Writer{}
	other := "0190a000-0000-7000-8000-00000000000b"
	if err := w.Append(engineLine(other)); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{threadA, "", "rec-s1", "../" + other, strings.ToUpper(other)} {
		if res := Lookup(session); res.State != Standalone {
			t.Errorf("%q: %v", session, res.State)
		}
	}
	if res := Lookup(other); res.State != Engine {
		t.Fatalf("registered: %v", res.State)
	}
	orch := Line{RunID: "run-1", ThreadID: threadA, Kind: KindOrchestrator, HelperPairs: map[string]Pair{HelperExplorer: {"m", "high"}, HelperReviewer: {"r", "high"}}}
	if err := w.Append(orch); err != nil {
		t.Fatal(err)
	}
	if res := Lookup(threadA); res.State != Orchestrator || res.Line.HelperPairs[HelperReviewer].Model != "r" || res.Line.NativeAllowance != nil {
		t.Fatalf("orchestrator: %+v", res)
	}
	if err := w.Append(Line{RunID: "run-1", ThreadID: threadA, Kind: KindClosed}); err != nil {
		t.Fatal(err)
	}
	if res := Lookup(threadA); res.State != Standalone {
		t.Fatalf("closed: %v", res.State)
	}
	// only the last line counts: a closed run that is opened again is bound again
	if err := w.Append(orch); err != nil {
		t.Fatal(err)
	}
	if res := Lookup(threadA); res.State != Orchestrator {
		t.Fatalf("reopened: %v", res.State)
	}
	// a directory that others can read makes the verdict untrustworthy, for every thread-shaped session
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{threadA, other, "0190a000-0000-7000-8000-00000000000f"} {
		if res := Lookup(session); res.State != Unavailable {
			t.Errorf("%s: %v", session, res.State)
		}
	}
	if res := Lookup("rec-s1"); res.State != Standalone {
		t.Errorf("a session that is not a thread id consults nothing: %v", res.State)
	}
}

func TestCheckDir(t *testing.T) {
	dir := home(t)
	if exists, err := CheckDir(dir); exists || err != nil {
		t.Fatalf("absent: %v %v", exists, err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// a parent that is a file: the directory cannot be there
	file := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if exists, err := CheckDir(filepath.Join(file, "flow-bound")); exists || err != nil {
		t.Fatalf("under a file: %v %v", exists, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if exists, err := CheckDir(dir); !exists || err != nil {
		t.Fatalf("safe: %v %v", exists, err)
	}
	for _, mode := range []os.FileMode{0o755, 0o750, 0o770, 0o500, 0o701} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if exists, err := CheckDir(dir); !exists || err == nil {
			t.Errorf("mode %v: %v %v", mode, exists, err)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	uid := UID
	UID = func() int { return os.Getuid() + 1 }
	defer func() { UID = uid }()
	if _, err := CheckDir(dir); err == nil {
		t.Fatal("a directory of another owner")
	}
}

// Of any number of claims for one call at once, exactly one is made, and nobody reads a half-written one.
func TestFlowBoundSameEventConcurrentDeliveryMakesOneClaim(t *testing.T) {
	dir := home(t)
	if err := (&Writer{}).Append(engineLine(threadA)); err != nil {
		t.Fatal(err)
	}
	for round := range 20 {
		call := fmt.Sprintf("c_%012d", round)
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				created, err := CreateClaim(dir, threadA, call, Claim{ToolUseID: fmt.Sprintf("tool-%d", i), Input: strings.Repeat("x", 100000)})
				if err != nil {
					t.Error(err)
					return
				}
				// whoever looks sees the whole claim
				if c, present, err := ReadClaim(dir, threadA, call); err != nil || !present || len(c.Input) != 100000 {
					t.Errorf("claim: %v %v %d", present, err, len(c.Input))
				}
				if created {
					mu.Lock()
					won++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if won != 1 {
			t.Fatalf("round %d: %d claims were made", round, won)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ClaimsDir, threadA))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("a temporary file is left: %s", e.Name())
		}
	}
}

func TestAnswerIsStoredWhole(t *testing.T) {
	dir := home(t)
	if err := (&Writer{}).Append(engineLine(threadA)); err != nil {
		t.Fatal(err)
	}
	if _, present, err := ReadAnswer(dir, threadA, callA); present || err != nil {
		t.Fatalf("%v %v", present, err)
	}
	if err := WriteAnswer(dir, threadA, callA, "answer\n"); err != nil {
		t.Fatal(err)
	}
	if err := WriteAnswer(dir, threadA, callA, "answer\n"); err != nil {
		t.Fatal(err)
	}
	if a, present, err := ReadAnswer(dir, threadA, callA); a != "answer\n" || !present || err != nil {
		t.Fatalf("%q %v %v", a, present, err)
	}
	for _, name := range []string{"../escape", ".x", "a/b"} {
		if err := WriteAnswer(dir, threadA, name, "x"); err == nil {
			t.Errorf("the call id %q names a file", name)
		}
	}
}

func TestLedgerKeepsOneEntryPerDecisionOfAToolUse(t *testing.T) {
	dir := home(t)
	if err := (&Writer{}).Append(engineLine(threadA)); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadDecisions(dir, threadA); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	d := Decision{RunID: "run-1", ThreadID: threadA, ThreadKind: KindEngine, CallID: callA, ToolUseID: "t1", Decision: DecisionAllowed, RequestedEffort: "low", AppliedModel: "m", AppliedEffort: "high"}
	for range 3 {
		if err := AppendDecision(dir, d); err != nil {
			t.Fatal(err)
		}
	}
	again := d
	again.RequestedEffort = "" // the same tool use delivered with the input its answer carried: the same decision
	if err := AppendDecision(dir, again); err != nil {
		t.Fatal(err)
	}
	refused := d
	refused.Decision, refused.Reason = DecisionRefused, ReasonNativeLimitRefused
	if err := AppendDecision(dir, refused); err != nil {
		t.Fatal(err)
	}
	noID := Decision{RunID: "run-1", ThreadID: threadA, ThreadKind: KindOrchestrator, Decision: DecisionAllowed}
	for range 2 {
		if err := AppendDecision(dir, noID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadDecisions(dir, threadA)
	if err != nil || len(got) != 3 || got[0] != d || got[1] != refused || got[2] != noID {
		t.Fatalf("%+v %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, DecisionsDir, threadA+FileSuffix))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(dir, DecisionsDir)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", info, err)
	}
}

// CRW-1207 correction round (d4): a ledger line is committed when its line feed is written. A cut tail is no decision: the reader
// leaves it out, the writer takes it off before it appends, and a whole line that is no decision stops both.
func TestLedgerCutTailIsNotADecision(t *testing.T) {
	dir := home(t)
	if err := (&Writer{}).Append(engineLine(threadA)); err != nil {
		t.Fatal(err)
	}
	d1 := Decision{RunID: "run-1", ThreadID: threadA, ThreadKind: KindEngine, CallID: callA, ToolUseID: "t1", Decision: DecisionAllowed, AppliedModel: "m", AppliedEffort: "high"}
	d2 := d1
	d2.ToolUseID = "t2"
	if err := AppendDecision(dir, d1); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, DecisionsDir, threadA+FileSuffix)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"run_id":"run-1","thread_id":"` + threadA + `","tool_use_id":"t-cut","deci`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, err := ReadDecisions(dir, threadA); err != nil || len(got) != 1 || got[0] != d1 {
		t.Fatalf("the reader with a cut tail: %+v %v", got, err)
	}
	if err := AppendDecision(dir, d2); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadDecisions(dir, threadA); err != nil || len(got) != 2 || got[0] != d1 || got[1] != d2 {
		t.Fatalf("after the next decision: %+v %v", got, err)
	}
	if raw, _ := os.ReadFile(file); strings.Contains(string(raw), "t-cut") || strings.Count(string(raw), "\n") != 2 {
		t.Fatalf("ledger bytes %q", raw)
	}
	// a tail that is a whole decision without its line feed is still not committed
	if err := os.WriteFile(file, []byte(`{"tool_use_id":"x","decision":"allowed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadDecisions(dir, threadA); err != nil || len(got) != 0 {
		t.Fatalf("an unterminated line: %+v %v", got, err)
	}
	// a whole line that is no decision: neither reads nor writes
	if err := os.WriteFile(file, []byte("{\"tool_use_id\":\"x\"}\nnot a decision\n"+`{"tool_use_id":"y"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDecisions(dir, threadA); err == nil {
		t.Fatal("the reader accepted a whole line that is no decision")
	}
	before, _ := os.ReadFile(file)
	if err := AppendDecision(dir, d2); err == nil {
		t.Fatal("the writer appended to a ledger the reader cannot read")
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("the ledger changed: %q", after)
	}
}

// CRW-1207 correction round (d3): the directory entries of the claim namespace are flushed, a failing flush is an error.
func TestClaimEntriesAreFlushedAndFailuresPropagate(t *testing.T) {
	dir := home(t)
	if err := (&Writer{}).Append(engineLine(threadA)); err != nil {
		t.Fatal(err)
	}
	var seen []string
	old := SyncEntry
	SyncEntry = func(label string, d *os.File) error { seen = append(seen, label); return d.Sync() }
	t.Cleanup(func() { SyncEntry = old })
	if created, err := CreateClaim(dir, threadA, callA, Claim{ToolUseID: "t", Input: "{}"}); err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	if err := WriteAnswer(dir, threadA, callA, "a\n"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(seen, " ")
	for _, want := range []string{"entry:claims", "entry:" + threadA, "entry:" + callA, "entry:" + callA + ".answer"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not flushed; flushed %v", want, seen)
		}
	}
	SyncEntry = func(l string, d *os.File) error { return fmt.Errorf("injected %s", l) }
	if _, err := CreateClaim(dir, threadA, "c_000000000003", Claim{ToolUseID: "t", Input: "{}"}); err == nil {
		t.Error("CreateClaim ignored a failing flush")
	}
	if err := WriteAnswer(dir, threadA, "c_000000000003", "a\n"); err == nil {
		t.Error("WriteAnswer ignored a failing flush")
	}
	if err := SyncClaims(dir, threadA); err == nil {
		t.Error("SyncClaims ignored a failing flush")
	}
}
