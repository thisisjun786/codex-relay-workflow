package record_test

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// A Go install entry carries what both fault sweepers read (location = <runtime>/bin, the
// directory a running relay reports for itself; environment; integrity; source) plus
// binaryDigest and target, and none of the interpreter fields.
func TestGoInstallEntryIsAdditive(t *testing.T) {
	restore := record.StampSourceTree(strings.Repeat("2", 40))
	defer restore()
	entry := record.GoInstall("/rt/bin-0.3.0-abc", "codex-session-relay", strings.Repeat("a", 64), "crw install", true)
	for key, want := range map[string]any{"location": "/rt/bin-0.3.0-abc/bin", "environment": "/rt/bin-0.3.0-abc", "entryPoint": "/rt/bin-0.3.0-abc/bin/codex-session-relay",
		"binaryDigest": strings.Repeat("a", 64), "integrity": strings.Repeat("a", 64), "target": record.Target()} {
		if got := record.Get(entry, key); got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	for _, python := range []string{"interpreter", "interpreterPath", "installMode"} {
		if _, ok := record.Lookup(entry, python); ok {
			t.Errorf("a Go entry carries %s", python)
		}
	}
	source := record.Get(entry, "source").(record.Object)
	if record.Get(source, "repositoryTree") != strings.Repeat("2", 40) || record.Get(source, "subdirectoryTree") != strings.Repeat("2", 40) {
		t.Fatalf("the stamped tree is not the source's tree: %s", golden.Canon(source))
	}
}

// A binary built without the stamp records null trees rather than a revision nobody
// measured; build information supplies the commit and cleanliness where it exists.
func TestSourceWithoutAStampIsNull(t *testing.T) {
	restore := record.StampSourceTree("")
	defer restore()
	source := record.Source()
	if record.Get(source, "repositoryTree") != nil || record.Get(source, "subdirectoryTree") != nil {
		t.Fatalf("an unstamped build claimed a tree: %s", golden.Canon(source))
	}
	info, _ := debug.ReadBuildInfo()
	revision := ""
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value
		}
	}
	if revision == "" && record.Get(source, "repositoryCommit") != nil {
		t.Fatalf("a commit was reported with no build information: %s", golden.Canon(source))
	}
	if restore := record.StampSourceTree("not-a-tree"); record.Get(record.Source(), "repositoryTree") != nil {
		restore()
		t.Fatal("a malformed stamp was recorded")
	} else {
		restore()
	}
}

func TestFileDigestIsTheBytesSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(path, []byte("abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := record.FileDigest(path); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(got)
	}
}
