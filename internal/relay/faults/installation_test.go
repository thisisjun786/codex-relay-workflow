package faults

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func installRecord(t *testing.T, home, location string, source map[string]any) string {
	t.Helper()
	path := filepath.Join(home, "codex-relay-workflow", "host-record.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"recordVersion": 1, "components": map[string]any{"codex-session-relay": map[string]any{"installs": []any{map[string]any{"location": location, "source": source}}}}}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func testSource() map[string]any {
	return map[string]any{"repositoryCommit": strings.Repeat("a", 40), "repositoryTree": strings.Repeat("b", 40), "subdirectoryTree": strings.Repeat("c", 40), "workingTreeClean": true}
}
func Test22_SweeperUsesInjectedHostRecord(t *testing.T) {
	location := t.TempDir()
	injected := installRecord(t, t.TempDir(), location, testSource())
	environment := t.TempDir()
	other := testSource()
	other["repositoryCommit"] = strings.Repeat("9", 40)
	installRecord(t, environment, location, other)
	t.Setenv("XDG_STATE_HOME", environment)
	sw := &Sweeper{HostRecordPath: injected, Installation: Installation{Package: "codex-session-relay", Version: "1", Location: location}}
	want := map[string]any{"package": "codex-session-relay", "version": "1", "location": location, "revisionRecord": injected, "revisionReason": nil, "revision": map[string]any{"repositoryCommit": strings.Repeat("a", 40), "repositoryTree": strings.Repeat("b", 40), "subdirectoryTree": strings.Repeat("c", 40), "workingTreeClean": true, "environment": nil, "integrity": nil}}
	if got := sw.installation(); !reflect.DeepEqual(got, want) {
		t.Fatalf("whole-output comparison diff: %s", noticeDifference("installation", want, got))
	}
}

func Test22_FLF_7_RevisionBelongsToThisInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", home)
	location := t.TempDir()
	sw := &Sweeper{HostRecordPath: testHostRecordPath(), Installation: Installation{Package: "codex-session-relay", Version: "1", Location: location}}
	path := installRecord(t, home, location, testSource())
	facts := sw.facts("expected", "actual", "impact", nil, nil)["observed"].(map[string]any)
	installation := facts["installation"].(map[string]any)
	revision := installation["revision"].(map[string]any)
	if revision["repositoryCommit"] != strings.Repeat("a", 40) || revision["subdirectoryTree"] != strings.Repeat("c", 40) || installation["revisionRecord"] != path {
		t.Fatalf("revision mismatch: %+v", installation)
	}
	_ = path
	installRecord(t, home, "/another/location", testSource())
	if got := sw.installation()["revision"]; got != nil {
		t.Fatalf("foreign revision: %+v", got)
	}
}
func Test22_FLF_8_UnknownAndDirtyRevisions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", home)
	location := t.TempDir()
	sw := &Sweeper{HostRecordPath: testHostRecordPath(), Installation: Installation{Package: "codex-session-relay", Version: "1", Location: location}}
	for _, source := range []map[string]any{nil, {"repositoryCommit": strings.Repeat("a", 40)}} {
		if source != nil {
			installRecord(t, home, location, source)
		}
		got := sw.installation()
		if got["revision"] != nil || got["revisionReason"] == nil {
			t.Fatalf("unknown revision: %+v", got)
		}
	}
	source := testSource()
	source["workingTreeClean"] = false
	installRecord(t, home, location, source)
	facts := sw.facts("e", "a", "i", nil, nil)["observed"].(map[string]any)
	if facts["installation"].(map[string]any)["revision"] == nil || !strings.Contains(strings.Join(anyStrings(facts["limits"]), " "), "uncommitted changes") {
		t.Fatalf("dirty revision: %+v", facts)
	}
}
func anyStrings(value any) []string {
	var result []string
	for _, entry := range value.([]any) {
		result = append(result, entry.(string))
	}
	return result
}
func Test22_FLF_9_ReplacedRecordIsReread(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", home)
	location := t.TempDir()
	sw := &Sweeper{HostRecordPath: testHostRecordPath(), Installation: Installation{Package: "codex-session-relay", Location: location}}
	path := installRecord(t, home, location, testSource())
	if sw.installation()["revision"].(map[string]any)["repositoryCommit"] != strings.Repeat("a", 40) {
		t.Fatal("initial revision")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	next := path + ".next"
	source := testSource()
	source["repositoryCommit"] = strings.Repeat("9", 40)
	raw, err := json.Marshal(map[string]any{"recordVersion": 1, "components": map[string]any{"codex-session-relay": map[string]any{"installs": []any{map[string]any{"location": location, "source": source}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(next, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(next, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	if got := sw.installation()["revision"].(map[string]any)["repositoryCommit"]; got != strings.Repeat("9", 40) {
		t.Fatalf("cached stale revision: %v", got)
	}
}
