package recall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListRolloutFilesReportsUnreadableDirectories(t *testing.T) {
	home := t.TempDir()
	hidden := writeRolloutTestFile(t, home, "sessions/2026/01/01/a.jsonl", ingestMessage(t, "retainedterm opening"))
	shown := writeRolloutTestFile(t, home, "sessions/2026/01/02/b.jsonl", ingestMessage(t, "otherterm opening"))
	dir := filepath.Dir(hidden)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("the user can read a mode-zero directory")
	}
	files, unread, err := listRolloutFiles(home, 0)
	if err != nil || len(files) != 1 || files[0].Path != shown || len(unread) != 1 || unread[0] != dir {
		t.Fatalf("files=%v unread=%v err=%v", files, unread, err)
	}
	if plain, err := ListRolloutFiles(home, 0); err != nil || len(plain) != 1 {
		t.Fatalf("%v %v", plain, err)
	}
}
