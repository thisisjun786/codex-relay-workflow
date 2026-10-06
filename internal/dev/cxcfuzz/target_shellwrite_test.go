//go:build dev

package cxcfuzz

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The target is registered under its name, with a Node shim and a comparator.
func TestShellwriteTargetIsRegistered(t *testing.T) {
	target, ok := Lookup("shellwrite")
	if !ok {
		t.Fatalf("shellwrite is not registered; registered: %v", Names())
	}
	if target.Oracle.Command != "node" || !strings.HasSuffix(target.Oracle.Shim, filepath.Join("testdata", "shellwrite", "shim.mjs")) {
		t.Fatalf("oracle %+v", target.Oracle)
	}
	if _, err := os.Stat(target.Oracle.Shim); err != nil {
		t.Fatalf("the shim is missing: %v", err)
	}
}

// Before the target is registered the command ends unknown target: this is the red-first case the
// issue names. The name is looked up in the table rather than through the command, so the test
// still holds after the entry exists (Lookup of a name that is not there).
func TestShellwriteUnknownTargetEndsTwo(t *testing.T) {
	if _, ok := Lookup("shellwrite-not-a-target"); ok {
		t.Fatal("a made-up target name is registered")
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"shellwrite-not-a-target", "--cases", "1"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "unknown target") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

// The generator produces a command object the Go side and the shim both read, over the whole
// range of sizes, and it never emits a destination outside the placeholder or the fixed pool.
func TestShellwriteGeneratorShape(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		input := shellWriteGenerate(rng, rng.Int())
		value, found := field(input, "command")
		command, ok := value.(string)
		if !found || !ok {
			t.Fatalf("input %s has no command string", canonical(input))
		}
		if strings.ContainsAny(command, "\x00") {
			t.Fatalf("command holds NUL: %q", command)
		}
		// The input must survive the JSON round trip the campaign makes, or the Go side and the
		// shim would read different commands.
		decoded, err := decode(canonical(input))
		if err != nil {
			t.Fatalf("the input is not JSON: %v", err)
		}
		again, found := field(decoded, "command")
		if !found || again != command {
			t.Fatalf("the command changed in the round trip: %q -> %v", command, again)
		}
	}
}

// The comparator is a destination set: order and duplicates do not matter, an oracle destination
// the port lacks is a miss, a port-only one is extra, and a difference on both sides is differ.
func TestShellwriteCompareClassifiesSets(t *testing.T) {
	for _, c := range []struct {
		name       string
		goSide     any
		oracleSide any
		want       Kind
	}{
		{"same order differs", []any{"b", "a"}, []any{"a", "b"}, Same},
		{"duplicates ignored", []any{"a", "a"}, []any{"a"}, Same},
		{"both empty", []any{}, []any{}, Same},
		{"oracle only is a miss", []any{}, []any{"/m/a"}, Miss},
		{"go only is extra", []any{"/m/a"}, []any{}, Extra},
		{"both ways is differ", []any{"/m/a"}, []any{"/m/b"}, Differ},
		{"a worker failure is not an empty set", pyjson.Object{{Key: "error", Value: "boom"}}, []any{}, Differ},
		{"a non-array answer is not an empty set", "boom", []any{}, Differ},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellWriteCompare(c.goSide, c.oracleSide).Kind; got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The Go side answers hook.ShellWriteDestinations of the command, with the case's own root in the
// place of the input's ${ROOT}, and a non-nil array even when nothing is named.
func TestShellwriteGoAnswersTheReader(t *testing.T) {
	root := t.TempDir()
	env := RootEnv(root)
	input := pyjson.Object{{Key: "command", Value: "echo hi > " + rootPlaceholder + "/m/n.md"}}
	got, err := shellWriteGo(input, env)
	if err != nil {
		t.Fatal(err)
	}
	if canonical(got) != `["`+root+`/m/n.md"]` {
		t.Fatalf("got %s", canonical(got))
	}
	empty, err := shellWriteGo(pyjson.Object{{Key: "command", Value: "true"}}, env)
	if err != nil || canonical(empty) != "[]" {
		t.Fatalf("empty: %s, %v", canonical(empty), err)
	}
}

// The same input names each side's own root: two different roots substitute the placeholder
// independently, so the harness can compare one input against two trees. Pinned here because the
// whole target depends on it and a campaign only sees it indirectly.
func TestShellwriteRootPlaceholderNamesEachSidesRoot(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	input := pyjson.Object{{Key: "command", Value: "echo hi > " + rootPlaceholder + "/m/n.md; tee " + rootPlaceholder + "/m/t"}}
	for _, root := range []string{one, two} {
		got, err := shellWriteGo(input, RootEnv(root))
		if err != nil {
			t.Fatal(err)
		}
		text := canonical(got)
		if !strings.Contains(text, root+"/m/n.md") || !strings.Contains(text, root+"/m/t") {
			t.Fatalf("root %s: %s", root, text)
		}
		if strings.Contains(text, rootPlaceholder) {
			t.Fatalf("the placeholder reached the answer: %s", text)
		}
	}
}

// The seed cases replay through the Go side with no Node and no worker, and each one still holds
// its tag: an identical case agrees with the oracle's recorded answer, an intentionally-changed
// case agrees with the Go answer its record names, and an open case still prints the Go output it
// was pinned with.
func TestShellwriteSeedCasesReplay(t *testing.T) {
	target, ok := Lookup("shellwrite")
	if !ok {
		t.Fatal("shellwrite is not registered")
	}
	cases, err := LoadCases(filepath.Join("testdata", "shellwrite"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 8 {
		t.Fatalf("%d seed cases, want at least 8", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if problem := CheckCase(target, c); problem != "" {
				t.Fatal(problem)
			}
		})
	}
}
