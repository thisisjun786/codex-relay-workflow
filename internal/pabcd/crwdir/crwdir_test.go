package crwdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// oracleIgnoreText is the .codexclaw/.gitignore text CXC v0.2.40 writes (state.test.ts IGNORE_TEXT).
const oracleIgnoreText = "# CodexClaw wrote this when it created .codexclaw; everything here is local runtime state.\n*\n!.gitignore\n!rules/\n!rules/*.md\n"

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEnsureDirPublishesExactBytesAndLeavesAnExistingFolderAlone(t *testing.T) {
	fresh, existing, linked, target := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	dir, err := EnsureDir(fresh)
	if err != nil || dir != filepath.Join(fresh, ".crw") || read(t, filepath.Join(dir, ".gitignore")) != GitignoreText {
		t.Fatalf("EnsureDir = %q, %v", dir, err)
	}
	if err := os.Mkdir(filepath.Join(existing, ".crw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linked, ".crw")); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{existing, linked} { // an existing folder, and a symlink in its place
		if _, err := EnsureDir(cwd); err != nil {
			t.Fatal(err)
		}
	}
	for _, left := range []string{filepath.Join(existing, ".crw"), target} {
		if entries, _ := os.ReadDir(left); len(entries) != 0 {
			t.Fatalf("%s was written: %v", left, entries)
		}
	}
}

// A creator that loses the race keeps the winner's bytes, whatever they are.
func TestEnsureDirKeepsTheWinnersIgnoreFile(t *testing.T) {
	for _, winner := range []string{GitignoreText, "other rules\n"} {
		cwd := t.TempDir()
		raced := func(path, _ string) error { return errors.Join(os.WriteFile(path, []byte(winner), 0o644), os.ErrExist) }
		if _, err := ensureDir(cwd, raced); err != nil || read(t, filepath.Join(cwd, ".crw", ".gitignore")) != winner {
			t.Fatalf("winner %q: %v", winner, err)
		}
	}
}

// A failed ignore write removes the empty directory it made and returns the error; it never removes
// what another writer put in its place (rmdirSync refuses a file, os.Remove would delete it).
func TestEnsureDirCleansUpOnlyItsOwnEmptyDirectory(t *testing.T) {
	denied := errors.New("denied")
	cwd := t.TempDir()
	if _, err := ensureDir(cwd, func(string, string) error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory survived a failed ignore write: %v", err)
	}
	cwd = t.TempDir()
	replaced := func(path, _ string) error {
		dir := filepath.Dir(path)
		return errors.Join(os.Remove(dir), os.WriteFile(dir, []byte("another writer"), 0o644), denied)
	}
	if _, err := ensureDir(cwd, replaced); !errors.Is(err, denied) || read(t, filepath.Join(cwd, ".crw")) != "another writer" {
		t.Fatalf("err = %v", err)
	}
}

// Every fixture of the CXC corpus that observes a first .codexclaw/.gitignore holds the oracle's
// bytes, and name-substitution rules R25 and R26 (the latter until stable) turn them into the constant.
func TestGitignoreTextMatchesTheCorpusFixtureTrees(t *testing.T) {
	root := filepath.Join("..", "..", "..", "contract")
	var rules struct {
		Rules []struct {
			ID, Regex, Replace string
			Repeat             bool
		}
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(root, "schema", "cxc", "name-substitution.json"))), &rules); err != nil {
		t.Fatal(err)
	}
	text := oracleIgnoreText
	for _, rule := range rules.Rules {
		if rule.ID == "R25" || rule.ID == "R26" {
			for pattern, again := regexp.MustCompile(rule.Regex), true; again; {
				next := pattern.ReplaceAllString(text, rule.Replace)
				again, text = rule.Repeat && next != text, next
			}
		}
	}
	if text != GitignoreText {
		t.Fatalf("substituted oracle text = %q", text)
	}
	names, _ := filepath.Glob(filepath.Join(root, "fixtures", "cxc", "*.json"))
	seen := 0
	for _, name := range names {
		raw := read(t, name)
		if !strings.Contains(raw, "CodexClaw wrote this") {
			continue
		}
		var fixture struct {
			Expect struct {
				Tree map[string]struct{ Text string }
			}
		}
		if err := json.Unmarshal([]byte(raw), &fixture); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for path, entry := range fixture.Expect.Tree {
			if strings.HasSuffix(path, ".codexclaw/.gitignore") {
				if seen++; entry.Text != oracleIgnoreText {
					t.Errorf("%s: %s holds %q", filepath.Base(name), path, entry.Text)
				}
			}
		}
	}
	if seen != 50 {
		t.Fatalf("%d fixture trees hold a .gitignore, want the 50 the corpus records", seen)
	}
}
