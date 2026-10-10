//go:build dev

package ci

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// CRW-1186 evaluation d5: the tool-version probes make their directories in TMPDIR while the current
// keys are collected, before the work root is looked at. A TMPDIR in the account's real home is
// refused before the first probe, for a fresh run and a reuse alike, and nothing is made there.
func TestLocalVerify_aTempDirInTheAccountHomeIsRefusedBeforeAnyProbe(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	tmp := filepath.Join(home, ".codex", "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	repo := newLocalFixture(t)
	for _, reuse := range []string{"", filepath.Join(t.TempDir(), "earlier.json")} {
		opts := localRunOptions(repo, localFixturePlan("true"), filepath.Join(t.TempDir(), "record.json"))
		_, _, err := localVerify(opts, reuse, io.Discard)
		var refusal *homeguard.Error
		if !errors.As(err, &refusal) {
			t.Errorf("reuse %q: want a refusal of the account home, got %v", reuse, err)
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Errorf("reuse %q: %d entries were made in the account's TMPDIR", reuse, len(entries))
		}
	}
	// the probe itself, called alone, makes nothing there either
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\necho 'go version go1.27.1 linux/amd64'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := localObserveTool("go", bin, ""); got != "" {
		t.Errorf("a probe in the account's TMPDIR answered %q", got)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("the probe made %d entries in the account's TMPDIR", len(entries))
	}
}

// CRW-1186 evaluation d4: --record may be any path; a record (and its .canonical sibling and the
// directories above them) in the account's real home is refused before the run, for a fresh run and
// a reuse alike. The real switch file is not replaced.
func TestLocalVerify_aRecordInTheAccountHomeIsRefused(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	dir := filepath.Join(home, ".codex", "crw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	switchFile := filepath.Join(dir, "switch.json")
	if err := os.WriteFile(switchFile, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	repo := newLocalFixture(t)
	for name, record := range map[string]string{
		"the switch file":   switchFile,
		"a new file":        filepath.Join(dir, "record.json"),
		"below a new dir":   filepath.Join(home, ".crw", "new", "record.json"),
		"through a link":    filepath.Join(link, "switch.json"),
		"through a dotdot":  link + "/../crw/switch.json",
		"runtime directory": filepath.Join(home, ".local", "share", "crw-runtime", "r.json"),
	} {
		for _, reuse := range []string{"", filepath.Join(t.TempDir(), "earlier.json")} {
			opts := localRunOptions(repo, localFixturePlan("true"), record)
			_, _, err := localVerify(opts, reuse, io.Discard)
			var refusal *homeguard.Error
			if !errors.As(err, &refusal) {
				t.Errorf("%s (reuse %q): want a refusal of the account home, got %v", name, reuse, err)
			}
		}
	}
	if raw, _ := os.ReadFile(switchFile); string(raw) != "original" {
		t.Errorf("the real switch file now holds %q", raw)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the account's crw directory holds %d entries, want only switch.json", len(entries))
	}
	for _, rel := range []string{".crw", ".local"} {
		if _, err := os.Lstat(filepath.Join(home, rel)); err == nil {
			t.Errorf("%s was made in the account home", rel)
		}
	}
}

// The checkout the record is written for may itself be a managed worktree below the account's .codex:
// a record inside that checkout is its own output and is written; one that leaves it for the account's
// protected directories, by name or through a link, is refused.
func TestLocalRecordDestination_aManagedCheckoutKeepsItsOwnRecord(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	checkout := filepath.Join(home, ".codex", "worktrees", "w")
	if err := os.MkdirAll(filepath.Join(home, ".codex", "crw"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex", "crw"), filepath.Join(checkout, "out")); err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{filepath.Join(checkout, localRecordDefault), filepath.Join(checkout, "new", "dir", "r.json")} {
		if err := localCheckRecordDestination(checkout, record); err != nil {
			t.Errorf("%s: %v", record, err)
		}
	}
	for _, record := range []string{
		filepath.Join(home, ".codex", "crw", "switch.json"),
		filepath.Join(checkout, "..", "..", "crw", "switch.json"),
		filepath.Join(checkout, "out", "switch.json"),
		filepath.Join(home, ".codex", "other", "r.json"),
	} {
		var refusal *homeguard.Error
		if err := localCheckRecordDestination(checkout, record); !errors.As(err, &refusal) {
			t.Errorf("%s: want a refusal of the account home, got %v", record, err)
		}
	}
}
