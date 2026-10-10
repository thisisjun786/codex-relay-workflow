//go:build dev

package ci

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1026 (verification round 3, d1): the record writer and the judge read one declaration. A go.mod spelling the go command
// reads (a single-line require, a comment on a block entry, CRLF, a quoted module path) and a scan_version assignment in any
// shell spelling give the pins the judge requires, so a record the writer seals for the commit is not refused for a pin the
// writer left out.
func TestLocalToolPinsAreTheOnesTheJudgeRequires(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"single-line require": {"go.mod": "module m\n\ngo 1.27\n\ntoolchain go1.27.1\n\nrequire honnef.co/go/tools v0.8.1\n"},
		"commented entry":     {"go.mod": "module m\n\ntoolchain go1.27.1\n\nrequire (\n\thonnef.co/go/tools v0.8.1 // staticcheck\n)\n"},
		"CRLF":                {"go.mod": "module m\r\n\r\ntoolchain go1.27.1\r\n\r\nrequire (\r\n\thonnef.co/go/tools v0.8.1\r\n)\r\n"},
		"quoted module path":  {"go.mod": "module m\n\ntoolchain go1.27.1\n\nrequire \"honnef.co/go/tools\" v0.8.1\n"},
		"block entry":         {"go.mod": "module m\n\ntoolchain go1.27.1\n\nrequire (\n\thonx.dev/x v1.0.0\n\thonnef.co/go/tools v0.8.1\n)\n"},
	} {
		for secrets, line := range map[string]string{"plain": "scan_version=8.30.1\n", "quoted": "scan_version='8.30.1' # pinned\n", "exported": "export scan_version=8.30.1\n"} {
			t.Run(name+"/"+secrets, func(t *testing.T) {
				files := map[string]string{"go.mod": files["go.mod"], "scripts/ci/secrets.sh": "#!/usr/bin/env bash\n" + line}
				read := func(path string) ([]byte, error) {
					body, ok := files[path]
					if !ok {
						return nil, os.ErrNotExist
					}
					return []byte(body), nil
				}
				pins, err := localToolPinsFrom(read)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]string{"go": "1.27.1", "staticcheck": "0.8.1", "gitleaks": "8.30.1"}
				for tool, version := range want {
					if pins[tool] != version {
						t.Errorf("the writer's pin %s is %q, want %q (pins %v)", tool, pins[tool], version, pins)
					}
				}
				judged, err := dagsched.DeclaredToolPins(func(path string) ([]byte, bool, error) {
					body, err := read(path)
					return body, err == nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for tool, version := range judged {
					if pins[tool] != version {
						t.Errorf("the judge requires %s %q and the writer pinned %q", tool, version, pins[tool])
					}
				}
			})
		}
	}
}

// A declaration the judge cannot read is an error of the writer too, never a record that pins less than the commit declares.
func TestLocalToolPinsRefuseADeclarationTheJudgeCannotRead(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"a staticcheck require without a version": {"go.mod": "module m\n\nrequire honnef.co/go/tools\n"},
		"two toolchains":          {"go.mod": "module m\n\ntoolchain go1.27.1\ntoolchain go1.26.0\n"},
		"a computed scan_version": {"scripts/ci/secrets.sh": "scan_version=$(cat VERSION)\n"},
	} {
		_, err := localToolPinsFrom(func(path string) ([]byte, error) {
			body, ok := files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(body), nil
		})
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// a tree without the declaring files pins nothing and is not an error
	if pins, err := localToolPinsFrom(func(string) ([]byte, error) { return nil, os.ErrNotExist }); err != nil || len(pins) != 0 {
		t.Fatalf("an empty tree pins nothing, got %v, %v", pins, err)
	}
}
