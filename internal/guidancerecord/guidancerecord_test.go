package guidancerecord

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envFor(home string) func(string) (string, bool) {
	return func(k string) (string, bool) { return home, k == "CODEX_HOME" }
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
