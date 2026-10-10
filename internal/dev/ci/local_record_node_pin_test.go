//go:build dev

package ci

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1191: the record writer and the judge read one declaration, node included. The judge (dagsched.DeclaredToolPins, which
// crw manage runtime-upgrade and the integrator's acceptance both use) requires a pin for every tool the verified commit
// declares; a writer that left out node sealed records the judge refused as disposition_conflict.

// localNodePinWorkflow is a ci.yml in the shape the repository's own has: a setup-node step whose condition (if:) comes before
// uses:, a quoted version and a comment inside the with block.
const localNodePinWorkflow = `name: fixture

on:
  push:

jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@0000000000000000000000000000000000000000 # v4
      - id: paths
        run: echo changed=true
      - if: steps.paths.outputs.changed == 'true' && steps.mirror.outputs.mirrored != 'true'
        uses: actions/setup-node@0000000000000000000000000000000000000000 # v4
        with:
          # An exact release, not the floating major.
          node-version: '24.20.0'
      - name: Say hello
        run: echo hello
`

func localTreeReader(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		body, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(body), nil
	}
}

// The writer's pins for the repository's own tree name every tool the judge requires of it, node among them.
func TestLocalToolPinsOfTheRepositoryTreeIncludeNode(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	read := func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(root, path)) }
	pins, err := localToolPinsFrom(read)
	if err != nil {
		t.Fatal(err)
	}
	if pins["node"] == "" {
		t.Fatalf("the writer's pins of this repository's tree have no node: %v", pins)
	}
	judged, err := dagsched.DeclaredToolPins(func(path string) ([]byte, bool, error) {
		body, err := read(path)
		return body, err == nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pins, judged) {
		t.Errorf("the writer pins %v and the judge requires %v", pins, judged)
	}
}

// The writer's pins for the shape of the current ci.yml (an if: before the setup-node step, a quoted version) carry node, and
// localPinsAt (the reuse recomputation) reads the very same set.
func TestLocalToolPinsCarryTheNodeOfAConditionalSetupNodeStep(t *testing.T) {
	read := localTreeReader(map[string]string{
		".github/workflows/ci.yml": localNodePinWorkflow,
		"go.mod":                   "module m\n\ntoolchain go1.27.1\n\nrequire honnef.co/go/tools v0.8.1\n",
		"scripts/ci/secrets.sh":    "scan_version=8.30.1\n",
	})
	pins, err := localToolPinsFrom(read)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"go": "1.27.1", "staticcheck": "0.8.1", "gitleaks": "8.30.1", "node": "24.20.0"}
	if !reflect.DeepEqual(pins, want) {
		t.Fatalf("the writer's pins are %v, want %v", pins, want)
	}
	at, _, err := localPinsAt(read)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(at, pins) {
		t.Errorf("the reuse recomputation pins %v and the writer pins %v", at, pins)
	}
}

// A tree without setup-node declares no node, and its pins carry none (CRW-1026: the pins are what the tree declares).
func TestLocalToolPinsOfATreeWithoutSetupNodeHaveNoNode(t *testing.T) {
	read := localTreeReader(map[string]string{
		".github/workflows/ci.yml": strings.Replace(localNodePinWorkflow, "uses: actions/setup-node@0000000000000000000000000000000000000000 # v4\n        with:\n          # An exact release, not the floating major.\n          node-version: '24.20.0'", "run: echo none", 1),
		"go.mod":                   "module m\n\ntoolchain go1.27.1\n",
	})
	pins, err := localToolPinsFrom(read)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pins["node"]; ok || pins["go"] != "1.27.1" {
		t.Fatalf("a tree without setup-node pins no node, got %v", pins)
	}
}

// fakeNodeBin is a PATH directory whose node answers version, ahead of the host's PATH.
func fakeNodeBin(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho v" + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// End to end: the record the writer seals for a tree is accepted by the judge runtime-upgrade uses, with node pinned when the
// tree declares it, and without node when the tree has no setup-node step.
func TestLocalRecordTheWriterSealsIsAcceptedByTheRuntimeUpgradeJudge(t *testing.T) {
	path := fakeNodeBin(t, "24.20.0")
	goVersion := localToolVersions(path)["go"]
	if goVersion == "" {
		t.Skip("no go on PATH")
	}
	for name, test := range map[string]struct {
		workflow string
		node     bool
	}{
		"a tree with a conditional setup-node step": {localNodePinWorkflow, true},
		"an older tree without setup-node":          {strings.Replace(localNodePinWorkflow, "uses: actions/setup-node@0000000000000000000000000000000000000000 # v4\n        with:\n          # An exact release, not the floating major.\n          node-version: '24.20.0'", "run: echo none", 1), false},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newLocalFixture(t)
			repo.write(".github/workflows/ci.yml", test.workflow)
			repo.write("go.mod", "module fixture\n\ngo 1.27\n\ntoolchain go"+goVersion+"\n\nrequire (\n\thonnef.co/go/tools v0.8.1\n)\n")
			repo.commit()
			plan := []localJob{{name: "validate", steps: []localStep{{name: "Say hello", kind: localRun, command: "echo hello", scope: "full"}}}}
			opts := localRunOptions(repo, plan, filepath.Join(t.TempDir(), "record.json"))
			opts.Env = []string{"PATH=" + path}
			made, _, err := localVerify(opts, "", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if made.Result != localPass || len(made.PinMismatch) > 0 {
				t.Fatalf("the run is %q with mismatches %v: %+v", made.Result, made.PinMismatch, made.Jobs)
			}
			if _, err := writeRecord(opts.Record, made); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(opts.Record)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := made.Pins["node"]; ok != test.node || (ok && got != "24.20.0") {
				t.Fatalf("the sealed record's node pin is %q (present %v), want present %v at 24.20.0: %v", got, ok, test.node, made.Pins)
			}
			keys, err := dagsched.CommitVerificationKeys(context.Background(), repo.root, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			keys.Base = made.BaseCommit
			keys.OS, keys.Arch = runtime.GOOS, runtime.GOARCH
			if _, ok := keys.Pins["node"]; ok != test.node {
				t.Fatalf("the judge's declaration of the tree is %v, want node present %v", keys.Pins, test.node)
			}
			if _, err := dagsched.JudgeVerificationRecord(raw, keys); err != nil {
				t.Fatalf("the judge refuses the record the writer sealed: %v", err)
			}
		})
	}
}

// The per-job node comparison reads the jobs parseWorkflow gives; the pins the record names come from the judge's reader. The
// two read the same node declarations of the repository's own ci.yml, and a comment line inside a setup-node step's with block
// (as the repository's ci.yml has) does not hide the node-version after it.
func TestLocalWorkflowNodeVersionsAreTheJudgesDeclaration(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"the repository's ci.yml": string(body), "a commented with block": localNodePinWorkflow} {
		jobs, err := parseWorkflow(text)
		if err != nil {
			t.Fatal(err)
		}
		var parsed []string
		for _, job := range jobs {
			for _, step := range job.steps {
				if step.nodeVersion != "" {
					parsed = append(parsed, step.nodeVersion)
				}
			}
		}
		declared, err := dagsched.DeclaredToolPins(func(path string) ([]byte, bool, error) {
			if path == ".github/workflows/ci.yml" {
				return []byte(text), true, nil
			}
			return nil, false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(localSortedUnique(parsed), ","); got == "" || got != declared["node"] {
			t.Errorf("%s: the jobs' node-version is %q and the judge's declaration is %q", name, got, declared["node"])
		}
	}
}
