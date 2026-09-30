package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The in-package half of oracle_support_test.go: the answers the Python reference gave these
// tests are read from recordings (pyoracle); capture runs the live Python only under
// CRW_PYTHON_ORACLE=record or check.

// pythonAnswer is one Python process's answer.
type pythonAnswer struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// oracleDir is this package's directory, where its recordings are.
var oracleDir = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}()

// askPython is pyoracle.JSON with every temporary directory the anchors name (and the
// repository root) stored as a placeholder, from a test that may have changed its working
// directory: the recording is read in the package directory and capture runs in the test's.
func askPython(t *testing.T, key string, out any, capture func() (any, error), anchors ...string) {
	t.Helper()
	options := []pyoracle.Option{pyoracle.SameWhen(func(recorded, live []byte) bool {
		return volatileText(recorded) == volatileText(live)
	})}
	repo := filepath.Clean(filepath.Join(oracleDir, "..", "..", ".."))
	options = append(options, pyoracle.Substitute(repo, "<repo>"))
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
		options = append(options, pyoracle.Substitute(prefix, fmt.Sprintf("<tmp%d>", i)))
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd != oracleDir {
		if pyoracle.CurrentMode() == pyoracle.Record {
			// Runs after the recording is written (see below).
			t.Cleanup(func() { _ = os.Chdir(wd) })
		}
		if err = os.Chdir(oracleDir); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chdir(wd); err != nil {
				t.Fatal(err)
			}
		}()
		inPackage := capture
		capture = func() (any, error) {
			if err := os.Chdir(wd); err != nil {
				return nil, err
			}
			defer func() { _ = os.Chdir(oracleDir) }()
			return inPackage()
		}
	}
	raw := pyoracle.Answer(t, key, func() ([]byte, error) {
		value, err := capture()
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}, options...)
	// Decoded as encoding/json decodes the Go side's answers (numbers as float64).
	if err = json.Unmarshal(raw, out); err != nil {
		t.Fatalf("pyoracle: decode %q: %v", key, err)
	}
	if wd != oracleDir && pyoracle.CurrentMode() == pyoracle.Record {
		// The recording is written when the test ends, relative to the working directory then:
		// this cleanup runs before that write, and the one above after it.
		t.Cleanup(func() { _ = os.Chdir(oracleDir) })
	}
}

var (
	volatileID    = regexp.MustCompile(`[0-9a-f]{32}|[0-9a-f]{16}`)
	volatileClock = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:[+-]\d\d:\d\d|Z)`)
)

// volatileText is a recorded answer without what a rerun changes: wall-clock instants, and
// generated identifiers renamed by first appearance (check mode compares answers so).
func volatileText(raw []byte) string {
	names := map[string]string{}
	return volatileID.ReplaceAllStringFunc(volatileClock.ReplaceAllString(string(raw), "<time>"), func(id string) string {
		if name, ok := names[id]; ok {
			return name
		}
		names[id] = fmt.Sprintf("<id%d>", len(names))
		return names[id]
	})
}
