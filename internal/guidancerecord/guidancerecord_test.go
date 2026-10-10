package guidancerecord

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envFor(home string) func(string) (string, bool) {
	return func(k string) (string, bool) { return home, k == "CODEX_HOME" }
}

// ownTurn is the transcript of a resume whose turn compacted before its SessionStart hooks ran: the evidence a pair needs (turn.go).
func ownTurn(t *testing.T) string {
	return newPairTranscript(t).turn("t1", true).path
}

func TestRecordedTextIsDeliveredAndOnlyThatText(t *testing.T) {
	env := envFor(t.TempDir())
	if Delivered(env, "s", "leg", "a", "") {
		t.Fatal("delivered before any record")
	}
	Record(env, "s", "leg", "a", "")
	if !Delivered(env, "s", "leg", "a", "") || Delivered(env, "s", "leg", "b", "") || Delivered(env, "t", "leg", "a", "") || Delivered(env, "s", "other", "a", "") {
		t.Fatal("a record answers for another text, session or leg")
	}
	Record(env, "s", "leg", "b", "")
	if Delivered(env, "s", "leg", "a", "") || !Delivered(env, "s", "leg", "b", "") {
		t.Fatal("a later record does not replace the earlier one")
	}
}

func TestTheCommandIsPartOfWhatWasDelivered(t *testing.T) {
	env := envFor(t.TempDir())
	Record(env, "s", "leg", "a", "/x/crw")
	if !Delivered(env, "s", "leg", "a", "/x/crw") || Delivered(env, "s", "leg", "a", "/y/crw") || Delivered(env, "s", "leg", "a", "") {
		t.Fatal("a record answers for another command")
	}
}

func TestUnusableKeysAreNeverDeliveredAndNeverWritten(t *testing.T) {
	home := t.TempDir()
	env := envFor(home)
	for _, c := range [][2]string{{"", "leg"}, {"a b", "leg"}, {"a\nb", "leg"}, {strings.Repeat("x", 257), "leg"}, {"s", ""}, {"s", "Leg"}, {"s", "../x"}} {
		Record(env, c[0], c[1], "a", "")
		if Delivered(env, c[0], c[1], "a", "") {
			t.Errorf("%q %q delivered", c[0], c[1])
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("unusable keys wrote %v", entries)
	}
	if Delivered(func(string) (string, bool) { return "", false }, "s", "leg", "a", "") {
		t.Error("delivered without a home")
	}
}

func TestACorruptOrLinkedRecordIsNotDelivered(t *testing.T) {
	home := t.TempDir()
	env := envFor(home)
	Record(env, "s", "leg", "a", "")
	path := slot(env, "s", "leg")
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil || Delivered(env, "s", "leg", "a", "") {
		t.Fatalf("garbage record: %v", err)
	}
	real := filepath.Join(home, "real")
	Record(env, "s", "leg", "a", "")
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(real, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if Delivered(env, "s", "leg", "a", "") {
		t.Error("a symlinked record was read")
	}
}

// The guard a test package installs keeps every record out of the account's real Codex home: a path there is neither read
// nor written (slot answers no path, before any file is touched), and the cleanup names it. Only slot is called with the real
// home, so this test itself touches nothing there even if the guard were broken.
func TestRefuseAccountHomeKeepsRecordsOutOfTheRealHome(t *testing.T) {
	account, err := user.Current()
	if err != nil || account.HomeDir == "" {
		t.Skip("no account home")
	}
	cleanup, err := RefuseAccountHome("")
	if err != nil || cleanup == nil {
		t.Fatalf("setup: %v", err)
	}
	real := envFor(filepath.Join(account.HomeDir, ".codex"))
	noHome := func(string) (string, bool) { return "", false } // falls back to the account home
	for _, env := range []func(string) (string, bool){real, noHome} {
		if p := slot(env, "s", "leg"); p != "" {
			_ = cleanup()
			t.Fatalf("a record path in the real home passed the guard: %s", p)
		}
	}
	temp := envFor(t.TempDir())
	Record(temp, "s", "leg", "a", "")
	if !Delivered(temp, "s", "leg", "a", "") {
		_ = cleanup()
		t.Fatal("the guard refused a temporary home")
	}
	err = cleanup()
	if err == nil || !strings.Contains(err.Error(), filepath.Join(account.HomeDir, ".codex", "crw", dirName)) {
		t.Fatalf("cleanup did not report the refused path: %v", err)
	}
	if p := slot(real, "s", "leg"); p == "" {
		t.Fatal("the guard outlived its cleanup")
	}
}

// CRW-1180: a resume that gave the whole text lets the compact that follows it in the same turn stay silent, once.
func TestCompactAfterAResumeRepeatsOnlyOncePerResume(t *testing.T) {
	env := envFor(t.TempDir())
	tr := ownTurn(t)
	if CompactRepeatsResume(env, "s", "leg", "a", "", tr) {
		t.Fatal("a compact with no record was taken for the pair")
	}
	Record(env, "s", "leg", "a", "") // a start or a compact: the whole text, no resume
	if CompactRepeatsResume(env, "s", "leg", "a", "", tr) {
		t.Fatal("a compact after a start without a resume was taken for the pair")
	}
	RecordResume(env, "s", "leg", "a", "", tr)
	if CompactRepeatsResume(env, "s", "leg", "b", "", tr) || CompactRepeatsResume(env, "t", "leg", "a", "", tr) || CompactRepeatsResume(env, "s", "other", "a", "", tr) {
		t.Fatal("a compact with other text, session or leg was taken for the pair")
	}
	if !CompactRepeatsResume(env, "s", "leg", "a", "", tr) {
		t.Fatal("the compact right after the resume was not taken for the pair")
	}
	if CompactRepeatsResume(env, "s", "leg", "a", "", tr) {
		t.Fatal("a second compact was taken for the same pair")
	}
	RecordResume(env, "s", "leg", "a", "", tr)
	Record(env, "s", "leg", "a", "") // a later whole output starts a new generation
	if CompactRepeatsResume(env, "s", "leg", "a", "", tr) {
		t.Fatal("a resume's mark survived a later whole output")
	}
	RecordResume(env, "s", "leg", "a", "", tr)
	ClearResume(env, "s", "leg")
	if CompactRepeatsResume(env, "s", "leg", "a", "", tr) {
		t.Fatal("a cleared mark was taken for the pair")
	}
}

// A compact long after the resume is a compaction of its own: what the resume said is no longer in the context.
func TestCompactLongAfterTheResumeIsNotThePair(t *testing.T) {
	env := envFor(t.TempDir())
	tr := ownTurn(t)
	RecordResume(env, "s", "leg", "a", "", tr)
	path := slot(env, "s", "leg") + ".resume"
	old := time.Now().Add(-2 * PairWindow)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if CompactRepeatsResume(env, "s", "leg", "a", "", tr) {
		t.Fatal("a compact after the window was taken for the pair")
	}
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("a stale mark was left behind")
	}
}

// CRW-1180 (verification P1): the pair is a resume and the compaction of its own turn. The first user prompt after the resume belongs to
// that turn (a second hook call for it too); a prompt of another turn ends the pair, and the window does not make a later turn one.
func TestUserPromptOfALaterTurnEndsThePair(t *testing.T) {
	env := envFor(t.TempDir())
	tr := ownTurn(t)
	paired := func() bool { return CompactRepeatsResume(env, "s", "leg", "a", "", tr) }
	RecordResume(env, "s", "leg", "a", "", tr)
	NoteUserPrompt(env, "s", "t1")
	NoteUserPrompt(env, "s", "t1")
	if !paired() {
		t.Fatal("the prompt of the resume's own turn ended the pair")
	}
	RecordResume(env, "s", "leg", "a", "", tr)
	NoteUserPrompt(env, "s", "t1")
	NoteUserPrompt(env, "s", "t2")
	if paired() {
		t.Fatal("a compact after a prompt of a later turn was taken for the pair")
	}
	RecordResume(env, "s", "leg", "a", "", tr)
	NoteUserPrompt(env, "s", "")
	NoteUserPrompt(env, "s", "")
	if paired() {
		t.Fatal("two prompts without turn ids left the pair open")
	}
	RecordResume(env, "s", "leg", "a", "", tr)
	NoteUserPrompt(env, "other", "t1")
	NoteUserPrompt(env, "other", "t2")
	NoteUserPrompt(env, "", "t3")
	if !paired() {
		t.Fatal("another session's prompts ended the pair")
	}
	// The count keeps the resume's time: the window runs from the resume, not from the prompt.
	RecordResume(env, "s", "leg", "a", "", tr)
	path := slot(env, "s", "leg") + resumeSuffix
	old := time.Now().Add(-2 * PairWindow)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	NoteUserPrompt(env, "s", "t1")
	if st, err := os.Lstat(path); err != nil || time.Since(st.ModTime()) < PairWindow {
		t.Fatalf("a prompt moved the resume's time: %v", err)
	}
	if paired() {
		t.Fatal("a stale pair was taken after a prompt")
	}
	// A mark that is not one of ours is no pair, and a prompt removes it.
	RecordResume(env, "s", "leg", "a", "", tr)
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	NoteUserPrompt(env, "s", "t1")
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("a corrupt mark survived a prompt")
	}
}

// A resume that gave only a part of the text (the session binding and the banner) leaves a pair of the part: the compact of its turn
// may leave exactly that part out.
func TestAShortResumeLeavesAPairOfThePart(t *testing.T) {
	env := envFor(t.TempDir())
	tr := ownTurn(t)
	Record(env, "s", "leg", "a", "")
	RecordResumePart(env, "s", "leg", "binding", tr)
	if kind, part := TakePair(env, "s", "leg", "b", "", tr); kind != PairNone || part != "" {
		t.Fatal("a pair was taken for other text")
	}
	kind, part := TakePair(env, "s", "leg", "a", "", tr)
	if kind != PairPart || part != PartDigest("binding") || part == "" {
		t.Fatalf("pair = %v %q", kind, part)
	}
	if kind, _ := TakePair(env, "s", "leg", "a", "", tr); kind != PairNone {
		t.Fatal("a part pair was taken twice")
	}
	if CompactRepeatsResume(env, "s", "leg", "a", "", tr) {
		t.Fatal("a part pair silenced a compact")
	}
	RecordResumePart(env, "s", "leg", "", tr)
	if kind, _ := TakePair(env, "s", "leg", "a", "", tr); kind != PairNone {
		t.Fatal("a resume that gave nothing left a pair")
	}
	// Begin ends a pair for any start that does not take it, whatever the hook says next.
	RecordResume(env, "s", "leg", "a", "", tr)
	if kind, _ := Begin(env, "s", "clear", "leg", "", "", tr); kind != PairNone {
		t.Fatal("a clear took the pair")
	}
	if kind, _ := TakePair(env, "s", "leg", "a", "", tr); kind != PairNone {
		t.Fatal("a clear that said nothing left the pair open")
	}
}

// CRW-1180 (verification round 2, P1): a prompt that counts against a pair while a compact takes it, or while a start ends it, must not
// bring the mark back: each resume pairs with at most one compact, and an ended pair stays ended.
func TestAPromptCannotBringBackATakenOrEndedPair(t *testing.T) {
	env := envFor(t.TempDir())
	tr := ownTurn(t)
	race := func(end func()) {
		start := make(chan struct{})
		done := make(chan struct{})
		go func() { defer close(done); <-start; NoteUserPrompt(env, "s", "own-turn") }()
		close(start)
		end()
		<-done
	}
	for i := 0; i < 1500; i++ {
		RecordResume(env, "s", "leg", "a", "", tr)
		var first Pair
		race(func() { first, _ = TakePair(env, "s", "leg", "a", "", tr) })
		if second, _ := TakePair(env, "s", "leg", "a", "", tr); first == PairWhole && second == PairWhole {
			t.Fatalf("iteration %d: a prompt brought back the pair a compact took, and a second compact took it again", i)
		}
		RecordResume(env, "s", "leg", "a", "", tr)
		race(func() { ClearResume(env, "s", "leg") })
		if _, err := os.Lstat(slot(env, "s", "leg") + resumeSuffix); err == nil {
			t.Fatalf("iteration %d: a prompt brought back the pair a start ended", i)
		}
	}
}

// The hooks are separate processes, so the pair holds across processes too: a prompt hook counting in a loop of its own never brings
// back the mark a compact took.
func TestAPromptOfAnotherProcessCannotBringBackATakenPair(t *testing.T) {
	home := t.TempDir()
	env := envFor(home)
	tr := ownTurn(t)
	stop := filepath.Join(t.TempDir(), "stop")
	ready := stop + ".ready"
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperPromptHookLoop$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GUIDANCERECORD_PROMPT_LOOP="+home+string(os.PathListSeparator)+stop)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(stop, nil, 0o600)
		if err := cmd.Wait(); err != nil {
			t.Errorf("prompt loop: %v", err)
		}
	}()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Lstat(ready); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("the prompt loop did not start")
		}
	}
	for i := 0; i < 1000; i++ {
		RecordResume(env, "s", "leg", "a", "", tr)
		// Let the other process read the fresh mark before this one takes it.
		for j := 0; j < i%7*50; j++ {
			_ = digest("x")
		}
		first, _ := TakePair(env, "s", "leg", "a", "", tr)
		time.Sleep(20 * time.Microsecond)
		if second, _ := TakePair(env, "s", "leg", "a", "", tr); first == PairWhole && second == PairWhole {
			t.Fatalf("iteration %d: another process's prompt brought back the pair a compact took", i)
		}
	}
}

// CRW-1180 (evaluation d2): a start that gives up waiting for the lock of a hook that still holds it ends the pair for good: the slow
// hook cannot bring the mark back, because a mark is written once and afterwards only deleted.
func TestAStartThatTimedOutOnTheLockEndsThePairForGood(t *testing.T) {
	env := envFor(t.TempDir())
	tr := ownTurn(t)
	wait := lockWait
	lockWait = 30 * time.Millisecond
	t.Cleanup(func() { lockWait, promptPause = wait, nil })
	RecordResume(env, "s", "leg", "a", "", tr)
	// The prompt hook holds the lock and is held up after it read the mark; the start that follows the resume cannot wait for it.
	promptPause = func() {
		promptPause = nil
		ClearResume(env, "s", "leg")
		if _, err := os.Lstat(slot(env, "s", "leg") + resumeSuffix); err == nil {
			t.Error("a start that could not take the lock left the pair open")
		}
	}
	NoteUserPrompt(env, "s", "t1")
	if kind, _ := TakePair(env, "s", "leg", "a", "", tr); kind != PairNone {
		t.Fatal("the prompt that was slow to count brought back the pair a start ended")
	}
	// The same for a compact that gave up on the lock.
	RecordResume(env, "s", "leg", "a", "", tr)
	promptPause = func() {
		promptPause = nil
		if kind, _ := TakePair(env, "s", "leg", "a", "", tr); kind != PairNone {
			t.Error("a compact that could not take the lock took the pair")
		}
	}
	NoteUserPrompt(env, "s", "t1")
	if kind, _ := TakePair(env, "s", "leg", "a", "", tr); kind != PairNone {
		t.Fatal("the prompt that was slow to count brought back the pair a compact dropped")
	}
	// A resume written while another hook holds the lock leaves no pair.
	unlock, ok := lockSession(filepath.Dir(slot(env, "s", "leg")), false)
	if !ok {
		t.Fatal("lock")
	}
	RecordResume(env, "s", "leg", "a", "", tr)
	unlock()
	if kind, _ := TakePair(env, "s", "leg", "a", "", tr); kind != PairNone {
		t.Fatal("a resume that could not take the lock left a pair")
	}
}

// CRW-1180 (evaluation d1, verification of 79c79c07): the pair is a resume and the compaction of its own turn, and the evidence is the
// session's transcript: the resume's turn compacted before the resume ran, and since then the transcript gained nothing of another turn, no
// prompt and no compaction. Without that evidence the compact says the text, whatever the prompt hook did or did not see.
func TestACompactNeedsTranscriptEvidenceOfTheResumesTurn(t *testing.T) {
	env := envFor(t.TempDir())
	whole := func(tr string) Pair {
		t.Helper()
		kind, _ := TakePair(env, "s", "leg", "a", "", tr)
		return kind
	}
	// The resume's own turn, as codex 0.154.0 writes it: the compaction, the resume's hooks, their answers, the compact's hooks. No prompt
	// hook has ever run for this session.
	own := newPairTranscript(t).turn("t1", true)
	RecordResume(env, "s", "leg", "a", "", own.path)
	own.said()
	if whole(own.path) != PairWhole {
		t.Fatal("the compact of the resume's own turn was not the pair")
	}
	// A later turn whose prompt the prompt hook missed (the hook ran for the resume's turn only).
	later := newPairTranscript(t).turn("t1", false)
	RecordResume(env, "s", "leg", "a", "", later.path)
	NoteUserPrompt(env, "s", "t1")
	later.said().end("t1").turn("t2", true)
	if whole(later.path) != PairNone {
		t.Fatal("the compact of a later turn whose prompt was missed was taken for the pair")
	}
	// The same when the prompt hook never runs at all (disabled), and when the resume's turn had compacted before it ran.
	again := newPairTranscript(t).turn("t1", true)
	RecordResume(env, "s", "leg", "a", "", again.path)
	again.said().end("t1").turn("t2", true)
	if whole(again.path) != PairNone {
		t.Fatal("the compact of a later turn without prompt hook was taken for the pair")
	}
	// A compaction later in the resume's own turn, after its prompt: the context lost what the resume said.
	mid := newPairTranscript(t).turn("t1", true)
	RecordResume(env, "s", "leg", "a", "", mid.path)
	mid.said().add(`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u"}}}`,
		`{"type":"compacted","payload":{"message":""}}`, `{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"ContextCompaction","id":"c2"}}}`)
	if whole(mid.path) != PairNone {
		t.Fatal("a compaction after the resume turn's prompt was taken for the pair")
	}
	// A resume whose turn did not compact before it ran (a resume between turns, or before its turn's compaction) leaves no pair.
	for _, tr := range []*pairTranscript{newPairTranscript(t).turn("t1", false), newPairTranscript(t).turn("t0", true).end("t0")} {
		RecordResume(env, "s", "leg", "a", "", tr.path)
		if _, err := os.Lstat(slot(env, "s", "leg") + resumeSuffix); err == nil {
			t.Fatal("a resume without the compaction of its turn left a mark")
		}
		tr.said().turn("t1", true)
		if whole(tr.path) != PairNone {
			t.Fatal("a compact after a resume without evidence was taken for the pair")
		}
	}
	// No transcript, an unreadable or other one, a compact that names another, and one that shrank are no evidence.
	RecordResume(env, "s", "leg", "a", "", "")
	if whole("") != PairNone {
		t.Fatal("a pair was taken without a transcript")
	}
	for _, tr := range []string{filepath.Join(t.TempDir(), "missing.jsonl"), t.TempDir()} {
		RecordResume(env, "s", "leg", "a", "", tr)
		if whole(tr) != PairNone {
			t.Fatalf("a pair was taken on %s", tr)
		}
	}
	other := newPairTranscript(t).turn("t1", true)
	RecordResume(env, "s", "leg", "a", "", other.path)
	if whole(newPairTranscript(t).turn("t1", true).path) != PairNone {
		t.Fatal("a compact naming another transcript was taken for the pair")
	}
	shrunk := newPairTranscript(t).turn("t1", true)
	RecordResume(env, "s", "leg", "a", "", shrunk.path)
	if err := os.WriteFile(shrunk.path, []byte(`{"type":"turn_context","payload":{"turn_id":"t1"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if whole(shrunk.path) != PairNone {
		t.Fatal("a pair was taken on a transcript that shrank")
	}
	// A record that does not parse, one still being written, or more than the bounded read since the resume is no evidence either.
	for _, tail := range []string{"not json\n", `{"type":"response_item"`, strings.Repeat(`{"type":"response_item","payload":{"type":"message","role":"developer"}}`+"\n", turnRange/60)} {
		tr := newPairTranscript(t).turn("t1", true)
		RecordResume(env, "s", "leg", "a", "", tr.path)
		f, err := os.OpenFile(tr.path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, werr := f.WriteString(tail)
		if cerr := f.Close(); werr != nil || cerr != nil {
			t.Fatal(werr, cerr)
		}
		if whole(tr.path) != PairNone {
			t.Fatalf("a pair was taken after %.40q", tail)
		}
	}
	// A part pair needs the same evidence.
	Record(env, "p", "leg", "a", "")
	RecordResumePart(env, "p", "leg", "binding", newPairTranscript(t).turn("t1", false).path)
	if kind, _ := TakePair(env, "p", "leg", "a", "", own.path); kind != PairNone {
		t.Fatal("a part pair was taken without evidence of the turn")
	}
	part := newPairTranscript(t).turn("t1", true)
	RecordResumePart(env, "p", "leg", "binding", part.path)
	if kind, d := TakePair(env, "p", "leg", "a", "", part.said().path); kind != PairPart || d != PartDigest("binding") {
		t.Fatalf("the part pair of the resume's own turn = %v %q", kind, d)
	}
}

// The resume reads only the end of a long transcript, and finds the compaction of its turn there; a compaction record longer than that
// end (the replacement history of a long session) does not hide it, because the event that names the turn comes after the record.
func TestAResumeFindsItsTurnAtTheEndOfALongTranscript(t *testing.T) {
	env := envFor(t.TempDir())
	tr := newPairTranscript(t).turn("t0", false).end("t0")
	tr.add(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"`+strings.Repeat("x", 2*turnTail)+`"}]}]}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"ContextCompaction","id":"c"}}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1"}}`)
	RecordResume(env, "s", "leg", "a", "", tr.path)
	tr.said()
	if kind, _ := TakePair(env, "s", "leg", "a", "", tr.path); kind != PairWhole {
		t.Fatal("the compact of the resume's own turn in a long transcript was not the pair")
	}
}

// TestHelperPromptHookLoop is the prompt hook of TestAPromptOfAnotherProcessCannotBringBackATakenPair; it runs only in that process.
func TestHelperPromptHookLoop(t *testing.T) {
	arg, ok := os.LookupEnv("GUIDANCERECORD_PROMPT_LOOP")
	if !ok {
		t.Skip("runs as the prompt hook of another test")
	}
	home, stop, _ := strings.Cut(arg, string(os.PathListSeparator))
	env := envFor(home)
	if err := os.WriteFile(stop+".ready", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
		if _, err := os.Lstat(stop); err == nil {
			return
		}
		NoteUserPrompt(env, "s", "own-turn")
	}
}

// pairTranscript is a session's Codex transcript for the CRW-1180 pair tests, in the record shapes of the isolated trial's codex 0.154.0
// rollouts: a turn that compacted before its SessionStart hooks ran, the answers the hooks gave, and the turns after it.
type pairTranscript struct {
	t    *testing.T
	path string
}

func newPairTranscript(t *testing.T) *pairTranscript {
	t.Helper()
	return &pairTranscript{t, filepath.Join(t.TempDir(), "rollout.jsonl")}
}

func (p *pairTranscript) add(records ...string) *pairTranscript {
	p.t.Helper()
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, record := range records {
		if _, err := f.WriteString(record + "\n"); err != nil {
			p.t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		p.t.Fatal(err)
	}
	return p
}

// turn is the start of a turn up to its context, with the compaction Codex ran before the SessionStart hooks when compacted is set.
func (p *pairTranscript) turn(id string, compacted bool) *pairTranscript {
	p.add(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + id + `"}}`)
	if compacted {
		p.add(`{"type":"compacted","payload":{"message":"","replacement_history":[]}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"`+id+`","item":{"type":"ContextCompaction","id":"c"}}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context></environment_context>"}]}}`)
	}
	return p.add(`{"type":"turn_context","payload":{"turn_id":"` + id + `"}}`)
}

// said is what Codex records of the SessionStart hooks' answers.
func (p *pairTranscript) said() *pairTranscript {
	return p.add(`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"[crw] guidance"}]}}`)
}

// end is the turn's user prompt, its answer and its end.
func (p *pairTranscript) end(id string) *pairTranscript {
	return p.add(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"`+id+`","item":{"type":"UserMessage","id":"u"}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"`+id+`"}}`)
}
