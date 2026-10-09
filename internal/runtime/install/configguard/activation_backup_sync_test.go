package configguard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// writeOnlyDir makes dir writable and searchable but not readable (mode 0300), so a file can be created and renamed
// in it while os.Open of the directory, and with it the directory fsync, is refused with a permission error.
func writeOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode 0300 directory")
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if d, err := os.Open(dir); err == nil {
		_ = d.Close()
		t.Skip("this file system opens a mode 0300 directory")
	}
}

// The backup is what a later restore reads, so a backup whose directory could not be synced stops the change before
// config.toml is rewritten: a directory that cannot be opened for the sync is not a durable backup (CRW-802).
func TestBackupReportsADirectoryThatCannotBeOpenedForTheSync(t *testing.T) {
	dir := t.TempDir()
	writeOnlyDir(t, dir)
	err := activationBackup(filepath.Join(dir, "config.toml.crw-x.bak"), []byte("a = 1\n"), 0o600)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("backup returned %v after its directory could not be opened for fsync", err)
	}
}

func TestActivateStopsBeforeTheConfigChangeWhenTheBackupCannotBeSynced(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	original := "# saved\n"
	activationWrite(t, path, original)
	writeOnlyDir(t, home)
	var calls [][]string
	m, err := Activate(activationDeps(t, home, map[string]bool{}, &calls))
	if !errors.Is(err, fs.ErrPermission) || m != nil {
		t.Fatalf("result=%+v error=%v", m, err)
	}
	for _, c := range calls {
		if len(c) > 1 && c[1] == "enable" {
			t.Fatalf("features enable ran after the unsynced backup: %v", calls)
		}
	}
	if got := activationRead(t, path); got != original {
		t.Fatalf("config.toml = %q", got)
	}
	if _, err := os.Stat(manifestPath(home)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("manifest after the refused activation: %v", err)
	}
}

func TestApplyManagedKeyStopsBeforeTheConfigChangeWhenTheBackupCannotBeSynced(t *testing.T) {
	home, path := configSetHome(t, configSetOriginal, true)
	manifest := activationRead(t, manifestPath(home))
	writeOnlyDir(t, home)
	value := true
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path, Now: func() string { return "2026-08-29T01:02:03.456Z" }}, configSetKey, &value)
	if !errors.Is(err, fs.ErrPermission) || r.OK {
		t.Fatalf("outcome %+v error %v", r, err)
	}
	if got := activationRead(t, path); got != configSetOriginal {
		t.Fatalf("config.toml = %q", got)
	}
	if got := activationRead(t, manifestPath(home)); got != manifest {
		t.Fatalf("manifest = %q", got)
	}
}
