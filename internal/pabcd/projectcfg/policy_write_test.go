package projectcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// settingsWithMore is a crw.json holding settings other than the interview policy, which a write must keep.
const settingsWithMore = `{"interview":"off","pabcd":{"enabled":false},"keep":[1,2,3]}`

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// A write that fails before the new file is published leaves the previous file whole. The directory is made
// read-only, so the temp file cannot be created while the settings file itself stays writable: an in-place
// write would still succeed there and replace the content, a write by rename cannot.
func TestWritePolicyFailureAtTheTempFileLeavesTheOldFile(t *testing.T) {
	dir := repoWith(t, settingsWithMore)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if probe, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		_ = probe.Close()
		t.Skip("this process can create files in a read-only directory (running as root?), so a failure at the temp file cannot be provoked")
	}
	res, err := WritePolicy(dir, PolicyAlways)
	if err == nil || res != (WriteResult{}) {
		t.Errorf("the temp file cannot be created, yet WritePolicy answered %+v, %v", res, err)
	}
	if got := readText(t, ConfigPath(dir)); got != settingsWithMore {
		t.Errorf("crw.json = %q, want it unchanged", got)
	}
	if names := entryNames(t, dir); len(names) != 1 || names[0] != ConfigFilename {
		t.Errorf("directory holds %v, want only %s", names, ConfigFilename)
	}
}

// A file that exists but cannot be read is not malformed: it is refused with a reason and left as it was.
func TestWritePolicyRefusesAFileItCannotRead(t *testing.T) {
	dir := repoWith(t, settingsWithMore)
	path := ConfigPath(dir)
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this process can read a mode 0200 file (running as root?), so an unreadable settings file cannot be provoked")
	}
	res, err := WritePolicy(dir, PolicyAlways)
	if err == nil || !strings.Contains(err.Error(), "could not read") || !strings.Contains(err.Error(), path) || res != (WriteResult{}) {
		t.Errorf("an unreadable file: %+v, %v", res, err)
	}
	if mode := modeOf(t, path); mode != 0o200 {
		t.Errorf("mode = %v, want 0200 kept", mode)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, path); got != settingsWithMore {
		t.Errorf("crw.json = %q, want it unchanged", got)
	}
	if names := entryNames(t, dir); len(names) != 1 {
		t.Errorf("directory holds %v, want only %s", names, ConfigFilename)
	}
}

// A file that was read and is not a JSON object is replaced and reported, whatever it held; its mode stays.
func TestWritePolicyReplacesAFileThatWasReadAndIsNotAnObject(t *testing.T) {
	for name, contents := range map[string]string{"truncated": `{"interview":"off","keep":`, "array": "[1]", "empty": ""} {
		dir := repoWith(t, contents)
		path := ConfigPath(dir)
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := WritePolicy(dir, PolicyAlways)
		if err != nil || !res.ReplacedMalformed || res.Path != path {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
		if got := readText(t, path); got != "{\n  \"interview\": \"always\"\n}\n" || modeOf(t, path) != 0o600 {
			t.Errorf("%s: crw.json = %q, mode %v", name, got, modeOf(t, path))
		}
	}
}

// The normal write keeps every other setting, an existing file's mode, and gives a new file the mode of any
// file this process creates (the oracle's writeFileSync opens with 0666 under the umask).
func TestWritePolicyKeepsSettingsAndModes(t *testing.T) {
	dir := repoWith(t, settingsWithMore)
	path := ConfigPath(dir)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if res, err := WritePolicy(dir, PolicyNewUnit); err != nil || res.ReplacedMalformed {
		t.Fatalf("write: %+v, %v", res, err)
	}
	want := "{\n  \"interview\": \"new-unit\",\n  \"pabcd\": {\n    \"enabled\": false\n  },\n  \"keep\": [\n    1,\n    2,\n    3\n  ]\n}\n"
	if got := readText(t, path); got != want || modeOf(t, path) != 0o640 {
		t.Errorf("crw.json = %q, mode %v", got, modeOf(t, path))
	}
	if names := entryNames(t, dir); len(names) != 1 {
		t.Errorf("directory holds %v, want only %s", names, ConfigFilename)
	}

	fresh := t.TempDir()
	control, err := os.OpenFile(filepath.Join(fresh, "control"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	_ = control.Close()
	if _, err := WritePolicy(fresh, PolicyOff); err != nil {
		t.Fatal(err)
	}
	if got, want := modeOf(t, ConfigPath(fresh)), modeOf(t, filepath.Join(fresh, "control")); got != want {
		t.Errorf("a new crw.json has mode %v, want %v", got, want)
	}
}

// The write follows a symlinked crw.json as the oracle's in-place write does: the link stays a link and its target changes.
func TestWritePolicyWritesThroughASymlink(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	target := filepath.Join(elsewhere, "shared.json")
	if err := os.WriteFile(target, []byte(settingsWithMore), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ConfigPath(dir)); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if _, err := WritePolicy(dir, PolicyAlways); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(ConfigPath(dir)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("crw.json is no longer a symlink: %v, %v", info, err)
	}
	if got := ReadPolicy(dir); got != PolicyAlways || !strings.Contains(readText(t, target), `"keep"`) {
		t.Errorf("target holds %q, policy %s", readText(t, target), got)
	}
}

// A file the owner made read-only is not replaced: the oracle's in-place write fails on it, and so does this one.
func TestWritePolicyRefusesAReadOnlyFile(t *testing.T) {
	dir := repoWith(t, settingsWithMore)
	path := ConfigPath(dir)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		_ = f.Close()
		t.Skip("this process can write a mode 0444 file (running as root?), so a read-only settings file cannot be provoked")
	}
	if res, err := WritePolicy(dir, PolicyAlways); err == nil || res != (WriteResult{}) {
		t.Errorf("a read-only file: %+v, %v", res, err)
	}
	if got := readText(t, path); got != settingsWithMore {
		t.Errorf("crw.json = %q, want it unchanged", got)
	}
}

// A reader that runs while the file is being rewritten sees the old document or the new one, never a part of either
// (the in-place write left an empty file between truncating and writing, which both readers answer with a default).
func TestWritePolicyIsNeverSeenTruncated(t *testing.T) {
	dir := repoWith(t, `{"interview":"off","pad":"`+strings.Repeat("x", 256<<10)+`"}`)
	var torn, reads atomic.Int64
	var started, stopOnce sync.Once
	running, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			data, err := os.ReadFile(ConfigPath(dir))
			reads.Add(1)
			if err != nil || !json.Valid(data) {
				torn.Add(1)
			}
			started.Do(func() { close(running) })
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	stopReader := func() {
		stopOnce.Do(func() { close(stop) })
		<-done
	}
	defer stopReader() // a failing write below must not leave the reader running
	<-running
	for i := 0; i < 100; i++ {
		if _, err := WritePolicy(dir, []Policy{PolicyAlways, PolicyOff}[i%2]); err != nil {
			t.Fatal(err)
		}
	}
	stopReader()
	if reads.Load() == 0 || torn.Load() != 0 {
		t.Errorf("%d of %d reads saw a missing or incomplete crw.json", torn.Load(), reads.Load())
	}
}
