package evidence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linkRefuseListing is every entry below dir with its content, so a test can show that nothing outside the workspace changed.
func linkRefuseListing(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != dir {
			data, _ := os.ReadFile(path)
			out = append(out, strings.TrimPrefix(path, dir)+"="+string(data))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// The attempt counter (in the flat directory and in the session's own one), the overflow record and the tombstone (through the
// session state and its lock) refuse a symbolic link in the state tree: they
// report that nothing was written, and the directory the link leads to outside the workspace is left as it was.
func TestEvidenceWritersRefuseALinkInTheStateTree(t *testing.T) {
	p := Payload{AgentType: "executor", AgentID: "a1", TurnID: "t1"}
	writers := map[string]func(cwd string) bool{
		"WriteAttempts":   func(cwd string) bool { return WriteAttempts(cwd, "s1", "a1", 1, "t1") },
		"RecordTombstone": func(cwd string) bool { return RecordTombstone(cwd, "s1", p, MaxAttempts, nil) },
		// An agent id that sanitising changes keeps its counter in the session's own directory (ownDirectoryCounter).
		"WriteAttempts own directory": func(cwd string) bool { return WriteAttempts(cwd, "s1", "a/1", 1, "t1") },
		"writeOverflow":               func(cwd string) bool { return writeOverflowAt(cwd, "s1", "a1", "t1") == nil },
	}
	for _, c := range []struct{ writer, link string }{
		{"WriteAttempts", ".crw"}, {"WriteAttempts", ".crw/" + AttemptsSubdir},
		{"RecordTombstone", ".crw"}, {"RecordTombstone", ".crw/sessions"},
		{"WriteAttempts own directory", ".crw/" + AttemptsSubdir},
		{"WriteAttempts own directory", ".crw/" + AttemptsSubdir + "/" + counterVersionDir},
		{"WriteAttempts own directory", ".crw/" + AttemptsSubdir + "/" + counterVersionDir + "/" + sessionRecordDir("s1")},
		{"writeOverflow", ".crw"}, {"writeOverflow", ".crw/" + OverflowSubdir},
		{"writeOverflow", ".crw/" + OverflowSubdir + "/" + sessionRecordDir("s1")},
	} {
		t.Run(c.writer+" "+c.link, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			link := filepath.Join(cwd, c.link)
			must(t, os.MkdirAll(filepath.Dir(link), 0o777))
			must(t, os.Symlink(outside, link))
			if writers[c.writer](cwd) {
				t.Fatal("the write reports success through a link")
			}
			if left := linkRefuseListing(t, outside); left != "" {
				t.Fatalf("written outside the workspace: %s", left)
			}
		})
	}
}

// An evidence root that is a symbolic link, or a state directory that is one, holds no valid receipt, even when the directory it
// leads to holds a non-empty regular file at the claimed name; the same file under a real root is valid.
func TestHasValidReceiptRefusesALinkedEvidenceRoot(t *testing.T) {
	for _, link := range []string{"", ".crw", ".crw/evidence"} {
		t.Run("link at "+link, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			real := filepath.Join(cwd, ".crw", "evidence")
			if link != "" { // the link leads to outside, which then holds the rest of the root's path
				rel, err := filepath.Rel(link, ".crw/evidence")
				must(t, err)
				real = filepath.Join(outside, rel)
				must(t, os.MkdirAll(filepath.Dir(filepath.Join(cwd, link)), 0o777))
				must(t, os.Symlink(outside, filepath.Join(cwd, link)))
			}
			must(t, os.MkdirAll(real, 0o777))
			must(t, os.WriteFile(filepath.Join(real, "proof.md"), []byte("ok"), 0o644))
			if got, want := HasValidReceipt(cwd, ".crw/evidence/proof.md"), link == ""; got != want {
				t.Fatalf("HasValidReceipt = %v, want %v", got, want)
			}
		})
	}
}
