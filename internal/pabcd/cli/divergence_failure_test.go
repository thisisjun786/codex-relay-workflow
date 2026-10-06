package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// TestDivergenceCliCandidateAddReportsTheFailedSavesNodeSpelling pins the oracle's Error.message for
// a failed candidate save. CXC v0.2.40's divergence-cli.ts:153 prints err.message, and the Node
// runtime spells a mkdir failure as "<NAME>: <description>, <op> '<path>'" (recorded with Node 24:
// "divergence candidate add: ENOTDIR: not a directory, mkdir '<cwd>/.codexclaw'", exit 1, nothing
// written). On dev this port answered Go's PathError spelling, "mkdir <cwd>/.crw: not a directory".
func TestDivergenceCliCandidateAddReportsTheFailedSavesNodeSpelling(t *testing.T) {
	cwd := divergenceCliSandbox(t)
	blocker := filepath.Join(cwd, "blocker")
	if err := os.WriteFile(blocker, []byte("a regular file where the workspace's .crw directory must be created\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := RunDivergenceCli([]string{
		"candidate", "add", "--session", "cli", "--cwd", blocker, "--kind", "alternative",
		"--title", "Streamed writer", "--rationale", "avoid buffering", "--source", "https://example.invalid/a",
	}, cwd)
	if err != nil {
		t.Fatalf("candidate add: %v", err)
	}
	want := "divergence candidate add: ENOTDIR: not a directory, mkdir '" + filepath.Join(blocker, crwdir.DirName) + "'"
	if result.Output != want || result.Code != 1 {
		t.Errorf("candidate add on a regular file: got %q (code %d), want %q (code 1)", result.Output, result.Code, want)
	}
	// The oracle left only the regular file; a refused save creates no .crw entry.
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == crwdir.DirName {
			t.Errorf("the refused save created %s", filepath.Join(cwd, crwdir.DirName))
		}
	}
}

// TestNodeErrorMessageSpellsWhatTheNodeRuntimePrints pins the shared helper's conversion: a
// *os.PathError whose errno planErrno names becomes the Node spelling, write and close leave the path
// out (the oracle's libuv messages carry none), and anything else keeps err.Error(). planFailure's
// recorded cases (plan_test.go:457) hold the write form of this text already.
func TestNodeErrorMessageSpellsWhatTheNodeRuntimePrints(t *testing.T) {
	type tc struct {
		name string
		err  error
		want string
	}
	unnamed := &os.PathError{Op: "open", Path: "/w/.crw/mode.json", Err: syscall.EBUSY}
	cases := []tc{
		{"an errno the table names", &os.PathError{Op: "mkdir", Path: "/w/.crw", Err: syscall.ENOTDIR}, "ENOTDIR: not a directory, mkdir '/w/.crw'"},
		{"write leaves the path out", &os.PathError{Op: "write", Path: "/w/.crw/mode.json", Err: syscall.ENOSPC}, "ENOSPC: no space left on device, write"},
		{"close leaves the path out", &os.PathError{Op: "close", Path: "/w/.crw/mode.json", Err: syscall.EIO}, "EIO: i/o error, close"},
		{"not a PathError", errors.New("boom"), "boom"},
		{"an errno the table does not name", unnamed, unnamed.Error()},
		{"a wrapped PathError", fmt.Errorf("candidate: %w", &os.PathError{Op: "mkdir", Path: "/w/.crw", Err: syscall.EACCES}), "EACCES: permission denied, mkdir '/w/.crw'"},
		{"a write past the file-size limit", &os.PathError{Op: "write", Path: "/w/.crw/divergence/candidates.jsonl", Err: syscall.EFBIG}, "EFBIG: file too large, write"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nodeErrorMessage(c.err); got != c.want {
				t.Errorf("nodeErrorMessage(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestDivergenceCliCandidateAddReportsTheFileSizeLimitFailure is the same text on the one path that
// reaches a write outside the helper's unit rows: a candidate row past RLIMIT_FSIZE. The oracle
// answers "divergence candidate add: EFBIG: file too large, write" (recorded with Node 24 and a
// 512-byte limit), and Go returns the EFBIG PathError rather than dying of the signal the limit
// raises, so the test lowers the limit for the process and restores it.
func TestDivergenceCliCandidateAddReportsTheFileSizeLimitFailure(t *testing.T) {
	cwd := divergenceCliSandbox(t)
	var before syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &before); err != nil {
		t.Skipf("RLIMIT_FSIZE is not available here: %v", err)
	}
	limit := before
	limit.Cur = 4096
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Skipf("cannot lower RLIMIT_FSIZE here: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &before); err != nil {
			t.Errorf("restoring RLIMIT_FSIZE: %v", err)
		}
	})
	result, err := RunDivergenceCli([]string{
		"candidate", "add", "--session", "cli", "--kind", "alternative",
		"--title", strings.Repeat("t", 8192), "--rationale", "past the file size limit",
		"--source", "https://example.invalid/a",
	}, cwd)
	if err != nil {
		t.Fatalf("candidate add: %v", err)
	}
	if want := "divergence candidate add: EFBIG: file too large, write"; result.Output != want || result.Code != 1 {
		t.Errorf("candidate add past RLIMIT_FSIZE: got %q (code %d), want %q (code 1)", result.Output, result.Code, want)
	}
}
