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

// promptHookRuns stamps the session as one whose user-prompt hook has run before: the session had a turn under the hooks, then a resume.
func promptHookRuns(env func(string) (string, bool), session string) {
	Record(env, session, "boot", "boot", "")
	NoteUserPrompt(env, session, "boot-turn")
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
	promptHookRuns(env, "s")
	if CompactRepeatsResume(env, "s", "leg", "a", "") {
		t.Fatal("a compact with no record was taken for the pair")
	}
	Record(env, "s", "leg", "a", "") // a start or a compact: the whole text, no resume
	if CompactRepeatsResume(env, "s", "leg", "a", "") {
		t.Fatal("a compact after a start without a resume was taken for the pair")
	}
	RecordResume(env, "s", "leg", "a", "")
	if CompactRepeatsResume(env, "s", "leg", "b", "") || CompactRepeatsResume(env, "t", "leg", "a", "") || CompactRepeatsResume(env, "s", "other", "a", "") {
		t.Fatal("a compact with other text, session or leg was taken for the pair")
	}
	if !CompactRepeatsResume(env, "s", "leg", "a", "") {
		t.Fatal("the compact right after the resume was not taken for the pair")
	}
	if CompactRepeatsResume(env, "s", "leg", "a", "") {
		t.Fatal("a second compact was taken for the same pair")
	}
	RecordResume(env, "s", "leg", "a", "")
	Record(env, "s", "leg", "a", "") // a later whole output starts a new generation
	if CompactRepeatsResume(env, "s", "leg", "a", "") {
		t.Fatal("a resume's mark survived a later whole output")
	}
	RecordResume(env, "s", "leg", "a", "")
	ClearResume(env, "s", "leg")
	if CompactRepeatsResume(env, "s", "leg", "a", "") {
		t.Fatal("a cleared mark was taken for the pair")
	}
}

// A compact long after the resume is a compaction of its own: what the resume said is no longer in the context.
func TestCompactLongAfterTheResumeIsNotThePair(t *testing.T) {
	env := envFor(t.TempDir())
	promptHookRuns(env, "s")
	RecordResume(env, "s", "leg", "a", "")
	path := slot(env, "s", "leg") + ".resume"
	old := time.Now().Add(-2 * PairWindow)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if CompactRepeatsResume(env, "s", "leg", "a", "") {
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
	promptHookRuns(env, "s")
	paired := func() bool { return CompactRepeatsResume(env, "s", "leg", "a", "") }
	RecordResume(env, "s", "leg", "a", "")
	NoteUserPrompt(env, "s", "t1")
	NoteUserPrompt(env, "s", "t1")
	if !paired() {
		t.Fatal("the prompt of the resume's own turn ended the pair")
	}
	RecordResume(env, "s", "leg", "a", "")
	NoteUserPrompt(env, "s", "t1")
	NoteUserPrompt(env, "s", "t2")
	if paired() {
		t.Fatal("a compact after a prompt of a later turn was taken for the pair")
	}
	RecordResume(env, "s", "leg", "a", "")
	NoteUserPrompt(env, "s", "")
	NoteUserPrompt(env, "s", "")
	if paired() {
		t.Fatal("two prompts without turn ids left the pair open")
	}
	RecordResume(env, "s", "leg", "a", "")
	NoteUserPrompt(env, "other", "t1")
	NoteUserPrompt(env, "other", "t2")
	NoteUserPrompt(env, "", "t3")
	if !paired() {
		t.Fatal("another session's prompts ended the pair")
	}
	// The count keeps the resume's time: the window runs from the resume, not from the prompt.
	RecordResume(env, "s", "leg", "a", "")
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
	RecordResume(env, "s", "leg", "a", "")
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
	promptHookRuns(env, "s")
	Record(env, "s", "leg", "a", "")
	RecordResumePart(env, "s", "leg", "binding")
	if kind, part := TakePair(env, "s", "leg", "b", ""); kind != PairNone || part != "" {
		t.Fatal("a pair was taken for other text")
	}
	kind, part := TakePair(env, "s", "leg", "a", "")
	if kind != PairPart || part != PartDigest("binding") || part == "" {
		t.Fatalf("pair = %v %q", kind, part)
	}
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairNone {
		t.Fatal("a part pair was taken twice")
	}
	if CompactRepeatsResume(env, "s", "leg", "a", "") {
		t.Fatal("a part pair silenced a compact")
	}
	RecordResumePart(env, "s", "leg", "")
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairNone {
		t.Fatal("a resume that gave nothing left a pair")
	}
	// Begin ends a pair for any start that does not take it, whatever the hook says next.
	RecordResume(env, "s", "leg", "a", "")
	if kind, _ := Begin(env, "s", "clear", "leg", "", ""); kind != PairNone {
		t.Fatal("a clear took the pair")
	}
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairNone {
		t.Fatal("a clear that said nothing left the pair open")
	}
}

// CRW-1180 (verification round 2, P1): a prompt that counts against a pair while a compact takes it, or while a start ends it, must not
// bring the mark back: each resume pairs with at most one compact, and an ended pair stays ended.
func TestAPromptCannotBringBackATakenOrEndedPair(t *testing.T) {
	env := envFor(t.TempDir())
	promptHookRuns(env, "s")
	race := func(end func()) {
		start := make(chan struct{})
		done := make(chan struct{})
		go func() { defer close(done); <-start; NoteUserPrompt(env, "s", "own-turn") }()
		close(start)
		end()
		<-done
	}
	for i := 0; i < 1500; i++ {
		RecordResume(env, "s", "leg", "a", "")
		var first Pair
		race(func() { first, _ = TakePair(env, "s", "leg", "a", "") })
		if second, _ := TakePair(env, "s", "leg", "a", ""); first == PairWhole && second == PairWhole {
			t.Fatalf("iteration %d: a prompt brought back the pair a compact took, and a second compact took it again", i)
		}
		RecordResume(env, "s", "leg", "a", "")
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
	promptHookRuns(env, "s")
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
		RecordResume(env, "s", "leg", "a", "")
		// Let the other process read the fresh mark before this one takes it.
		for j := 0; j < i%7*50; j++ {
			_ = digest("x")
		}
		first, _ := TakePair(env, "s", "leg", "a", "")
		time.Sleep(20 * time.Microsecond)
		if second, _ := TakePair(env, "s", "leg", "a", ""); first == PairWhole && second == PairWhole {
			t.Fatalf("iteration %d: another process's prompt brought back the pair a compact took", i)
		}
	}
}

// CRW-1180 (evaluation d2): a start that gives up waiting for the lock of a hook that still holds it ends the pair for good: the slow
// hook cannot bring the mark back, because a mark is written once and afterwards only deleted.
func TestAStartThatTimedOutOnTheLockEndsThePairForGood(t *testing.T) {
	env := envFor(t.TempDir())
	promptHookRuns(env, "s")
	wait := lockWait
	lockWait = 30 * time.Millisecond
	t.Cleanup(func() { lockWait, promptPause = wait, nil })
	RecordResume(env, "s", "leg", "a", "")
	// The prompt hook holds the lock and is held up after it read the mark; the start that follows the resume cannot wait for it.
	promptPause = func() {
		promptPause = nil
		ClearResume(env, "s", "leg")
		if _, err := os.Lstat(slot(env, "s", "leg") + resumeSuffix); err == nil {
			t.Error("a start that could not take the lock left the pair open")
		}
	}
	NoteUserPrompt(env, "s", "t1")
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairNone {
		t.Fatal("the prompt that was slow to count brought back the pair a start ended")
	}
	// The same for a compact that gave up on the lock.
	RecordResume(env, "s", "leg", "a", "")
	promptPause = func() {
		promptPause = nil
		if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairNone {
			t.Error("a compact that could not take the lock took the pair")
		}
	}
	NoteUserPrompt(env, "s", "t1")
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairNone {
		t.Fatal("the prompt that was slow to count brought back the pair a compact dropped")
	}
	// A resume written while another hook holds the lock leaves no pair.
	unlock, ok := lockSession(filepath.Dir(slot(env, "s", "leg")), false)
	if !ok {
		t.Fatal("lock")
	}
	RecordResume(env, "s", "leg", "a", "")
	unlock()
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairNone {
		t.Fatal("a resume that could not take the lock left a pair")
	}
}

// CRW-1180 (evaluation d1): the pair is a resume and the compaction of its own turn, and only the prompts tell a turn from the next one.
// A session whose prompt hook has not run (untrusted, disabled, failing) has no such evidence, so its compacts say the text; one whose
// hook has run, in this turn or before the resume, is paired as usual.
func TestACompactNeedsEvidenceThatThePromptHookRuns(t *testing.T) {
	env := envFor(t.TempDir())
	Record(env, "s", "leg", "a", "") // the session's start, under hooks whose prompt leg never ran
	RecordResume(env, "s", "leg", "a", "")
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairNone {
		t.Fatal("a compact was taken for the pair of a session whose prompt hook never ran")
	}
	if _, err := os.Lstat(slot(env, "s", "leg") + resumeSuffix); err == nil {
		t.Fatal("the mark of a session without prompt evidence was left behind")
	}
	// The hook ran in the turn of the resume.
	RecordResume(env, "s", "leg", "a", "")
	NoteUserPrompt(env, "s", "t1")
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairWhole {
		t.Fatal("the compact after the resume's own prompt was not the pair")
	}
	// The hook ran before the resume, and the compact comes before the resume's first prompt.
	RecordResume(env, "s", "leg", "a", "")
	if kind, _ := TakePair(env, "s", "leg", "a", ""); kind != PairWhole {
		t.Fatal("a session whose prompt hook ran earlier lost its pair")
	}
	// A part pair needs the same evidence.
	other := envFor(t.TempDir())
	Record(other, "s", "leg", "a", "")
	RecordResumePart(other, "s", "leg", "binding")
	if kind, _ := TakePair(other, "s", "leg", "a", ""); kind != PairNone {
		t.Fatal("a part pair was taken without prompt evidence")
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
