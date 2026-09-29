package faults

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostrecord "github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// The Go installer's entry (internal/runtime/hostrecord.GoInstall) is found by the sweeper that a
// Go relay running from <runtime>/bin/crw builds (Location = filepath.Dir of its resolved
// executable), and its source block is what the sweeper reads as the installed revision. An
// unstamped build's null trees are reported as an incomplete revision naming exactly those
// keys, never as a revision.
func Test37_SweeperReadsTheGoInstallEntry(t *testing.T) {
	home := t.TempDir()
	environment := filepath.Join(t.TempDir(), "bin-0.3.0-aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(environment, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "codex-relay-workflow", hostrecord.Name)
	digest := strings.Repeat("a", 64)
	entry := hostrecord.GoInstall(environment, "codex-session-relay", digest, "crw install", true)
	write := func(entry hostrecord.Object) {
		if err := hostrecord.Save(path, hostrecord.PutInstall(hostrecord.Empty(1), "codex-session-relay", entry)); err != nil {
			t.Fatal(err)
		}
	}
	sw := &Sweeper{HostRecordPath: path, Installation: Installation{Package: "codex-session-relay", Version: "1", Location: filepath.Join(environment, "bin")}}

	unstamped := hostrecord.Set(append(hostrecord.Object{}, entry...), "source", hostrecord.Object{
		{Key: "repositoryCommit", Value: strings.Repeat("1", 40)}, {Key: "repositoryTree", Value: nil},
		{Key: "subdirectoryTree", Value: nil}, {Key: "workingTreeClean", Value: true}})
	write(unstamped)
	if got := sw.installation(); got["revision"] != nil || got["revisionReason"] != "this copy's install entry records an incomplete revision (repositoryTree, subdirectoryTree missing or malformed), which identifies nothing" {
		t.Fatalf("an unstamped Go entry: %+v", got)
	}

	stamped := hostrecord.Set(append(hostrecord.Object{}, entry...), "source", hostrecord.Object{
		{Key: "repositoryCommit", Value: strings.Repeat("1", 40)}, {Key: "repositoryTree", Value: strings.Repeat("2", 40)},
		{Key: "subdirectoryTree", Value: strings.Repeat("2", 40)}, {Key: "workingTreeClean", Value: false}})
	write(stamped)
	got := sw.installation()
	revision, ok := got["revision"].(map[string]any)
	if !ok || revision["environment"] != environment || revision["integrity"] != digest || revision["repositoryTree"] != strings.Repeat("2", 40) || revision["workingTreeClean"] != false {
		t.Fatalf("a stamped Go entry: %+v", got)
	}
}
