//go:build dev

package cxcfuzz

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// CRW-1186: the fuzz harness makes its case roots, worker roots and output below directories of its
// own; it writes nothing in the account's real home, however HOME, CODEX_HOME or TMPDIR are set.

func fakeAccount(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	return home
}

func untouched(t *testing.T, home string) {
	t.Helper()
	for _, rel := range []string{".codex", ".crw", ".local/share/crw-runtime"} {
		if _, err := os.Lstat(filepath.Join(home, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists in the account home (%v)", rel, err)
		}
	}
}

func wantRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *homeguard.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("want a refusal of the account home, got %v", err)
	}
}

func TestPrepareRoot_refusesTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	wantRefusal(t, PrepareRoot(filepath.Join(home, ".codex", "case")))
	untouched(t, home)
}

func TestSaveCases_refusesTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	wantRefusal(t, SaveCases(filepath.Join(home, ".crw", "cases"), []Case{{Name: "x"}}))
	untouched(t, home)
}

func TestCampaign_refusesAnOutputInTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	_, err := Campaign(Config{Out: filepath.Join(home, ".local", "share", "crw-runtime", "fuzz")})
	wantRefusal(t, err)
	untouched(t, home)
}

func TestRun_refusesAnOutputInTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"echo", "--cases", "1", "--out", filepath.Join(home, ".codex", "fuzz")}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "account's real home") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	untouched(t, home)
}

func TestTemporaryRoots_neverLieInTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	tmp := filepath.Join(home, ".codex", "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	_, err := MkdirTempRoot("cxcfuzz-case-")
	wantRefusal(t, err)
	echo, ok := Lookup("echo")
	if !ok {
		t.Fatal("no echo target")
	}
	if problem := CheckCase(echo, Case{Input: "{}", Tag: TagIdentical}); !strings.Contains(problem, "account's real home") {
		t.Errorf("CheckCase in a TMPDIR of the account's .codex: %q", problem)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("%d entries were made in the account's .codex", len(entries))
	}
}
