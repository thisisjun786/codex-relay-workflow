package manage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The installer cleans the paths it is given (install.absolute is filepath.Abs): --from is read at
// the cleaned spelling and its SHA256SUMS is taken from that spelling's directory, and
// --backup-state-to is cleaned the same way. A run directory spelled through a link and ".." is
// kept raw by the management side, so the arguments of the update must name the directory the
// kernel resolves, or the installer reads (or fails to find) a different archive than the one the
// run pinned and verified, and writes the backup elsewhere.
func TestUpgradeUpdateArgumentsNameThePhysicalRunDirectory(t *testing.T) {
	t.Run("no file at the cleaned spelling", func(t *testing.T) { upgradeUpdateArgumentsCase(t, false) })
	t.Run("another archive and sums at the cleaned spelling", func(t *testing.T) { upgradeUpdateArgumentsCase(t, true) })
}

func upgradeUpdateArgumentsCase(t *testing.T, decoy bool) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, produceRuntime: true, pointAtIt: true})
	h.spellManageState()
	// the place a cleaned spelling of the run directory names: an archive and sums the run never verified
	wrong := filepath.Join(h.home, "manage-state", "upgrades")
	if decoy {
		dir := filepath.Join(wrong, h.now.UTC().Format("20060102T150405Z"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{upgradeArchiveName, upgradeSumsName} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("not the verified bytes "+name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	} else if _, err := os.Lstat(wrong); err == nil {
		t.Fatalf("%s exists before the run", wrong)
	}
	if code := h.run("--release-dir", h.release); code != 0 {
		t.Fatalf("exit %d; the record is %+v", code, h.recordOf(t))
	}
	var update string
	for _, line := range h.callArgs() {
		if strings.HasPrefix(line, "install update ") {
			update = line
		}
	}
	if update == "" {
		t.Fatalf("no install update call in %q", h.callArgs())
	}
	fields := strings.Fields(update)
	value := func(option string) string {
		for i, field := range fields {
			if field == option && i+1 < len(fields) {
				return fields[i+1]
			}
		}
		t.Fatalf("%s is not an argument of %q", option, update)
		return ""
	}
	from, backup := value("--from"), value("--backup-state-to")

	// what the installer reads is what the run pinned
	cleaned, err := filepath.Abs(from)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := os.ReadFile(filepath.Join(h.manageRoot(), "upgrades", h.recordDirs()[0], filepath.Base(from)))
	if err != nil {
		t.Fatalf("the pinned archive: %v", err)
	}
	read, err := os.ReadFile(cleaned)
	if err != nil {
		t.Fatalf("--from %s, which the installer reads as %s, names no archive: %v", from, cleaned, err)
	}
	if string(read) != string(pinned) {
		t.Errorf("--from %s reads other bytes than the pinned archive", from)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cleaned), upgradeSumsName)); err != nil {
		t.Errorf("the installer takes its checksums from %s: %v", filepath.Dir(cleaned), err)
	}
	// the backup is written where the run directory is
	cleanedBackup, err := filepath.Abs(backup)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(h.manageRoot(), "upgrades", h.recordDirs()[0], "state-backup"); cleanedBackup != want {
		t.Errorf("--backup-state-to %s is cleaned to %s, want %s", backup, cleanedBackup, want)
	}
	if _, err := os.Lstat(wrong); err == nil && !decoy {
		t.Errorf("a command created %s, the place a cleaned spelling of the state directory names", wrong)
	}
}
