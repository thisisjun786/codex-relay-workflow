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

// CRW-1186 verification round 1: a link below the destination that leads into the account's home.
func TestSaveCases_refusesACasesFileThatIsALinkIntoTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	protected := filepath.Join(home, ".crw", CasesFile)
	if err := os.MkdirAll(filepath.Dir(protected), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protected, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := os.Symlink(protected, filepath.Join(dest, CasesFile)); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, SaveCases(dest, nil))
	if raw, _ := os.ReadFile(protected); string(raw) != "original" {
		t.Errorf("the account's file reads %q", raw)
	}
}

func TestPrepareRoot_refusesAHomeBelowTheRootThatIsALinkIntoTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	env := RootEnv(root)
	if err := os.MkdirAll(filepath.Dir(env.CodexHome), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex"), env.CodexHome); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, PrepareRoot(root))
	if entries, _ := os.ReadDir(filepath.Join(home, ".codex")); len(entries) != 0 {
		t.Errorf("the account's .codex holds %d entries", len(entries))
	}
}

func TestOutputFiles_refuseALinkIntoTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.Symlink(filepath.Join(home, ".codex"), filepath.Join(out, DivergenceDir)); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, writeDivergence(out, Divergence{Kind: "x", Input: "{}"}, map[string]bool{}))
	out2 := t.TempDir()
	if err := os.Symlink(filepath.Join(home, ".codex", "summary.json"), filepath.Join(out2, "summary.json")); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, writeSummary(out2, Summary{}))
	if entries, _ := os.ReadDir(filepath.Join(home, ".codex")); len(entries) != 0 {
		t.Errorf("the account's .codex holds %d entries", len(entries))
	}
}
