package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The in-package half of golden_support_test.go: the answers these tests expect are goldens
// (internal/testsupport/golden), which began as the Python reference implementation's recorded
// answers (todo 44).

// packageDirectory is this package's directory.
var packageDirectory = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}()

// expectGolden checks value, the part of an answer a test compares, against the golden under
// key, with the repository root and every temporary directory the anchors (and HOME) name
// stored as placeholders. A test that changed its working directory is moved back to the package
// directory, where its goldens are, around the check.
func expectGolden(t *testing.T, key string, value any, anchors ...string) {
	t.Helper()
	repo := filepath.Clean(filepath.Join(packageDirectory, "..", "..", ".."))
	options := []golden.Option{golden.Substitute(repo, "<repo>")}
	tmp := filepath.Clean(os.TempDir())
	seen := map[string]bool{}
	var prefixes []string
	for _, anchor := range append(anchors, os.Getenv("HOME")) {
		for rest := anchor; ; {
			i := strings.Index(rest, tmp+"/")
			if i < 0 {
				break
			}
			rest = rest[i+len(tmp)+1:]
			component, _, _ := strings.Cut(rest, "/")
			prefix := tmp + "/" + component
			if component == "" || seen[prefix] || strings.HasPrefix(repo+"/", prefix+"/") {
				continue
			}
			seen[prefix] = true
			prefixes = append(prefixes, prefix)
		}
	}
	for i, prefix := range prefixes {
		options = append(options, golden.Substitute(prefix, fmt.Sprintf("<tmp%d>", i)))
	}
	inDirectory(t, packageDirectory, func() { golden.CheckJSON(t, key, value, options...) })
}

// inDirectory runs check, a golden check, in dir, the package directory, for a test that
// changed its working directory: a golden file is read and written relative to the working
// directory, and an update writes it when the test ends, so the directory is dir then too.
func inDirectory(t testing.TB, dir string, check func()) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd == dir {
		check()
		return
	}
	if golden.Updating() {
		// Runs after the golden file is written (see below).
		t.Cleanup(func() { _ = os.Chdir(wd) })
	}
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatal(err)
		}
	}()
	check()
	if golden.Updating() {
		// The golden file is written when the test ends, relative to the working directory then:
		// this cleanup runs before that write, and the one above after it.
		t.Cleanup(func() { _ = os.Chdir(dir) })
	}
}
