package crwdir

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestRenameReplacesTheDestinationAndSurfacesAFailure(t *testing.T) {
	dir := t.TempDir()
	tmp, final := filepath.Join(dir, "tmp"), filepath.Join(dir, "final")
	for path, text := range map[string]string{tmp: "new", final: "old"} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Rename(tmp, final); err != nil || read(t, final) != "new" {
		t.Fatalf("Rename = %v, final %q", err, read(t, final))
	}
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Rename(tmp, filepath.Join(dir, "missing", "final")); !errors.Is(err, os.ErrNotExist) || read(t, tmp) != "x" {
		t.Fatalf("a failed rename: %v, source %q", err, read(t, tmp))
	}
}

// atomic-write.test.ts "POSIX never retries": even EBUSY, which only win32 treats as transient, is
// returned after exactly one attempt.
func TestRenameNeverRetries(t *testing.T) {
	calls := 0
	err := renameWith(func(string, string) error { calls++; return syscall.EBUSY }, "tmp", "final")
	if !errors.Is(err, syscall.EBUSY) || calls != 1 {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
}

// withFile is a directory holding one file with the given content and mode.
func withFile(t *testing.T, content string, mode os.FileMode) (dir, final string) {
	t.Helper()
	dir = t.TempDir()
	final = filepath.Join(dir, "crw.json")
	if err := os.WriteFile(final, []byte(content), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(final, mode); err != nil {
		t.Fatal(err)
	}
	return dir, final
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

func temps(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestPublishReplacesOrCreatesTheFileAndLeavesNothingElse(t *testing.T) {
	dir, final := withFile(t, "old", 0o644)
	if err := Publish(final, []byte("new")); err != nil || read(t, final) != "new" || !slices.Equal(names(t, dir), []string{"crw.json"}) {
		t.Fatalf("replace: %v, %q, %v", err, read(t, final), names(t, dir))
	}
	fresh := filepath.Join(dir, "fresh.json")
	if err := Publish(fresh, []byte("made")); err != nil || read(t, fresh) != "made" || !slices.Equal(names(t, dir), []string{"crw.json", "fresh.json"}) {
		t.Fatalf("create: %v, %q, %v", err, read(t, fresh), names(t, dir))
	}
	if err := Publish(filepath.Join(dir, "missing", "crw.json"), []byte("x")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing directory: %v", err)
	}
}

// An existing file keeps its mode, as the in-place write kept it; a new file gets the mode any file created with 0666 gets.
func TestPublishModes(t *testing.T) {
	dir, final := withFile(t, "old", 0o640)
	if err := Publish(final, []byte("new")); err != nil || modeOf(t, final) != 0o640 {
		t.Fatalf("existing: %v, mode %v", err, modeOf(t, final))
	}
	control := filepath.Join(dir, "control")
	f, err := os.OpenFile(control, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	fresh := filepath.Join(dir, "fresh.json")
	if err := Publish(fresh, []byte("made")); err != nil || modeOf(t, fresh) != modeOf(t, control) {
		t.Fatalf("new: %v, mode %v, want %v", err, modeOf(t, fresh), modeOf(t, control))
	}
}

// A failure at any step leaves the previous file, its mode and its directory as they were: the temp file is gone. The hook runs
// just before the step, so the temp file is there for every step after the create.
func TestPublishFailureAtAnyStepLeavesTheOldFile(t *testing.T) {
	injected := errors.New("injected")
	for step, name := range map[publishStep]string{stepCreate: "create", stepMode: "mode", stepWrite: "write", stepSync: "sync", stepRename: "rename"} {
		dir, final := withFile(t, "old", 0o640)
		wantTemps := 1
		if step == stepCreate {
			wantTemps = 0
		}
		err := publish(final, []byte("new"), func(at publishStep) error {
			if at != step {
				return nil
			}
			if got := len(temps(t, dir)); got != wantTemps {
				t.Errorf("%s: %d temp files while the step fails, want %d", name, got, wantTemps)
			}
			return injected
		})
		if !errors.Is(err, injected) || read(t, final) != "old" || modeOf(t, final) != 0o640 || !slices.Equal(names(t, dir), []string{"crw.json"}) {
			t.Errorf("%s: err %v, content %q, mode %v, directory %v", name, err, read(t, final), modeOf(t, final), names(t, dir))
		}
	}
}

// A private file's new content is never readable through a broader temp: the temp of an existing file is created 0600 and
// only then given the file's own mode, and a file that was 0640 ends 0640.
func TestPublishTempOfAnExistingFileStartsPrivate(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640} {
		dir, final := withFile(t, "old", mode)
		seen := map[publishStep]os.FileMode{}
		err := publish(final, []byte("new"), func(at publishStep) error {
			if found := temps(t, dir); len(found) == 1 {
				seen[at] = modeOf(t, found[0])
			}
			return nil
		})
		if err != nil || seen[stepMode]&0o077 != 0 || seen[stepWrite] != mode || modeOf(t, final) != mode {
			t.Errorf("%v: %v, temp mode at creation %v and before the write %v, final %v", mode, err, seen[stepMode], seen[stepWrite], modeOf(t, final))
		}
	}
}

// The final path may be a symlink: the in-place write followed it, so the link stays and its target changes.
func TestPublishFollowsASymlink(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	target, link := filepath.Join(elsewhere, "shared.json"), filepath.Join(dir, "crw.json")
	if err := os.WriteFile(target, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if err := Publish(link, []byte("new")); err != nil || read(t, target) != "new" || modeOf(t, target) != 0o640 {
		t.Fatalf("%v, target %q", err, read(t, target))
	}
	if got, err := os.Readlink(link); err != nil || got != target || !slices.Equal(names(t, dir), []string{"crw.json"}) || !slices.Equal(names(t, elsewhere), []string{"shared.json"}) {
		t.Errorf("link %q, %v; directories %v and %v", got, err, names(t, dir), names(t, elsewhere))
	}
}

// What is not a regular file, and a link that leads nowhere, are refused with nothing touched.
func TestPublishRefusesWhatItCannotReplace(t *testing.T) {
	dir := t.TempDir()
	folder, dangling := filepath.Join(dir, "folder"), filepath.Join(dir, "dangling")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	for name, path := range map[string]string{"a directory": folder, "a dangling link": dangling} {
		if err := Publish(path, []byte("x")); err == nil {
			t.Errorf("%s: published", name)
		}
	}
	if info, err := os.Lstat(dangling); err != nil || info.Mode()&os.ModeSymlink == 0 || !slices.Equal(names(t, dir), []string{"dangling", "folder"}) || len(names(t, folder)) != 0 {
		t.Errorf("the refused paths changed: %v, %v", err, names(t, dir))
	}
}

// A file the owner made read-only is refused, as the in-place write fails on it.
func TestPublishRefusesAFileThatCannotBeWritten(t *testing.T) {
	dir, final := withFile(t, "old", 0o444)
	if f, err := os.OpenFile(final, os.O_WRONLY, 0); err == nil {
		_ = f.Close()
		t.Skip("this process can write a mode 0444 file (running as root?), so a read-only file cannot be provoked")
	}
	if err := Publish(final, []byte("new")); !errors.Is(err, os.ErrPermission) || read(t, final) != "old" || !slices.Equal(names(t, dir), []string{"crw.json"}) {
		t.Errorf("%v, %q, %v", err, read(t, final), names(t, dir))
	}
}

// A temp file that cannot be removed is not hidden: the failure reports it. The directory loses its write permission after
// the temp file exists, so neither the rename nor the removal can happen; the previous file stays whole.
func TestPublishReportsATempFileItCannotRemove(t *testing.T) {
	dir, final := withFile(t, "old", 0o644)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	err := publish(final, []byte("new"), func(at publishStep) error {
		if at == stepRename {
			return os.Chmod(dir, 0o555)
		}
		return nil
	})
	if probe, perr := os.Create(filepath.Join(dir, "probe")); perr == nil {
		_ = probe.Close()
		t.Skip("this process can write to a read-only directory (running as root?), so the removal cannot be made to fail")
	}
	if err == nil || !errors.Is(err, os.ErrPermission) || read(t, final) != "old" || len(temps(t, dir)) != 1 {
		t.Errorf("err %v, content %q, temp files %v", err, read(t, final), temps(t, dir))
	}
}

// Publish fsyncs the directory after the rename, as the last step: the file is in place under its final name when the sync is called, and a failed sync is returned as a PublishedError (the rename has
// happened, so the caller must not rely on the file being durable but must not undo it either; CRW-802).
func TestPublishSyncsTheDirectoryAfterTheRename(t *testing.T) {
	dir, final := withFile(t, "old", 0o640)
	var order []publishStep
	err := publish(final, []byte("new"), func(at publishStep) error {
		order = append(order, at)
		if at == stepDirSync && read(t, final) != "new" {
			t.Errorf("the directory is synced before the rename: %q", read(t, final))
		}
		return nil
	})
	want := []publishStep{stepCreate, stepMode, stepWrite, stepSync, stepRename, stepDirSync}
	if err != nil || !slices.Equal(order, want) || read(t, final) != "new" || !slices.Equal(names(t, dir), []string{"crw.json"}) {
		t.Fatalf("err %v, steps %v, want %v, content %q, directory %v", err, order, want, read(t, final), names(t, dir))
	}
}

func TestPublishReportsAFailedDirectorySyncAsPublished(t *testing.T) {
	injected := errors.New("injected")
	dir, final := withFile(t, "old", 0o640)
	err := publish(final, []byte("new"), func(at publishStep) error {
		if at == stepDirSync {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) || !Published(err) || read(t, final) != "new" || !slices.Equal(names(t, dir), []string{"crw.json"}) {
		t.Fatalf("err %v (published %v), content %q, directory %v", err, Published(err), read(t, final), names(t, dir))
	}
}

// A failure before the rename is not a publication: nothing is in place and the error is not a PublishedError.
func TestPublishFailureBeforeTheRenameIsNotPublished(t *testing.T) {
	injected := errors.New("injected")
	dir, final := withFile(t, "old", 0o640)
	for _, step := range []publishStep{stepCreate, stepMode, stepWrite, stepSync, stepRename} {
		err := publish(final, []byte("new"), func(at publishStep) error {
			if at == step {
				return injected
			}
			return nil
		})
		if !errors.Is(err, injected) || Published(err) || read(t, final) != "old" || !slices.Equal(names(t, dir), []string{"crw.json"}) {
			t.Errorf("step %d: err %v (published %v), content %q, directory %v", step, err, Published(err), read(t, final), names(t, dir))
		}
	}
}

// PublishChecked and PublishContext reach the same directory sync, and a cancellation at the rename step returns before it.
func TestPublishCheckedAndContextSyncTheDirectory(t *testing.T) {
	_, final := withFile(t, "old", 0o640)
	synced := false
	err := publishContext(context.Background(), final, []byte("ctx"), func(at publishStep) error {
		synced = synced || at == stepDirSync
		return nil
	})
	if err != nil || !synced || read(t, final) != "ctx" {
		t.Fatalf("PublishContext: err %v, synced %v, content %q", err, synced, read(t, final))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	synced = false
	if err := publishContext(ctx, final, []byte("late"), func(at publishStep) error {
		synced = synced || at == stepDirSync
		return nil
	}); !errors.Is(err, context.Canceled) || Published(err) || synced || read(t, final) != "ctx" {
		t.Fatalf("a cancelled PublishContext: err %v (published %v), synced %v, content %q", err, Published(err), synced, read(t, final))
	}
	if err := PublishChecked(final, []byte("checked"), func() error { return nil }); err != nil || read(t, final) != "checked" {
		t.Fatalf("PublishChecked: err %v, content %q", err, read(t, final))
	}
}

// A directory that takes writes but cannot be opened for reading cannot be fsynced: the rename has happened, so every publish
// entry point reports the failed open as a PublishedError (the file is in place, its entry is not known to be durable) and
// none of them reports success for a sync that did not run (CRW-802).
func TestPublishInAWriteOnlyDirectoryReportsTheUnsyncedDirectory(t *testing.T) {
	entries := map[string]func(final string) error{
		"Publish":        func(final string) error { return Publish(final, []byte("new")) },
		"PublishChecked": func(final string) error { return PublishChecked(final, []byte("new"), func() error { return nil }) },
		"PublishContext": func(final string) error { return PublishContext(context.Background(), final, []byte("new")) },
		"PublishDurable": func(final string) error { return PublishDurable(final, []byte("new")) },
	}
	for name, run := range entries {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o300); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			if _, err := os.ReadDir(dir); err == nil {
				t.Skip("the directory stays readable despite mode 0300 (privileged user)")
			}
			final := filepath.Join(dir, "crw.json")
			err := run(final)
			if !Published(err) || !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("%s in a write-only directory: err %v (published %v, permission %v), want a PublishedError for the failed directory open", name, err, Published(err), errors.Is(err, fs.ErrPermission))
			}
			if err := os.Chmod(dir, 0o755); err != nil || read(t, final) != "new" || len(temps(t, dir)) != 0 {
				t.Fatalf("chmod %v, content %q, temp files %v", err, read(t, final), temps(t, dir))
			}
		})
	}
}

func TestPublishDurableCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "new.json")
	if err := PublishDurable(final, []byte("a")); err != nil || read(t, final) != "a" {
		t.Fatalf("create: %v %q", err, read(t, final))
	}
	if err := PublishDurable(final, []byte("b")); err != nil || read(t, final) != "b" || len(temps(t, dir)) != 0 {
		t.Fatalf("replace: %v %q", err, read(t, final))
	}
}

func TestSyncDirOpensAndSyncsADirectoryAndRefusesAMissingOne(t *testing.T) {
	dir := t.TempDir()
	if err := SyncDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing directory: %v", err)
	}
}
