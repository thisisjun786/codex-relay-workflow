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
