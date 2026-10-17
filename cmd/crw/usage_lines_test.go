package main

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// CRW-1323 items 27 and 31 (the 10-13 trial): `crw --help` did not list flow, and `crw flow` with no command
// or an unknown one listed the engine's commands only (no start, no show) and answered `crw flow status` with
// five choices. A usage line names the commands that are registered, all of them and no others.

// registeredModes are the canonical words: exclude option spellings and the one deprecated alias.
func registeredModes() (names []string) {
	for _, m := range modes() {
		if !strings.HasPrefix(m.name, "-") && m.name != "pabcd" {
			names = append(names, m.name)
		}
	}
	return names
}

func TestUsageLineListsEveryRegisteredMode(t *testing.T) {
	var out, errOut strings.Builder
	if code := run(context.Background(), "crw", []string{"--help"}, &out, &errOut); code != 0 {
		t.Fatalf("crw --help: %d %q", code, errOut.String())
	}
	m := regexp.MustCompile(`usage: crw \[-h\] \[--version\] \{([^}]*)\} \.\.\.`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("crw --help prints no usage line: %q", out.String())
	}
	if got, want := strings.Split(m[1], ","), registeredModes(); !slices.Equal(got, want) {
		t.Errorf("the usage line lists %v, the mode table registers %v", got, want)
	}
	if !slices.Contains(strings.Split(m[1], ","), "flow") {
		t.Error("the usage line does not list flow")
	}
}

// flowUsageWords are the command words of the `crw flow <word>` usage lines in text, in order of appearance.
func flowUsageWords(text string) []string {
	var words []string
	for _, m := range regexp.MustCompile(`(?m)^(?:usage:)?\s*crw flow ([a-z][a-z-]*)`).FindAllStringSubmatch(text, -1) {
		if !slices.Contains(words, m[1]) {
			words = append(words, m[1])
		}
	}
	slices.Sort(words)
	return words
}

// TestFlowUsageLinesAreTheRegisteredCommands: `crw flow --help`, `crw flow` with no command and `crw flow status`
// name the same commands, the ones `crw flow` serves (TestFlowHelpIsTheDispatchTable holds --help to the tables).
func TestFlowUsageLinesAreTheRegisteredCommands(t *testing.T) {
	var out, errOut strings.Builder
	if code := run(context.Background(), "crw", []string{"flow", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("crw flow --help: %d %q", code, errOut.String())
	}
	want := flowUsageWords(out.String())
	// next and dispatched are the owner thread's side of a session run (CRW-1348); plan-input builds a handoff (CRW-1343).
	for _, w := range []string{"start", "show", "ps", "run", "compile", "lint", "reports", "trace", "next", "dispatched", "plan-input"} {
		if !slices.Contains(want, w) {
			t.Fatalf("crw flow --help names %v, without %s", want, w)
		}
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), "crw", []string{"flow"}, &out, &errOut); code != 2 {
		t.Errorf("crw flow: %d, want 2", code)
	}
	if got := flowUsageWords(errOut.String()); !slices.Equal(got, want) {
		t.Errorf("crw flow (no command) names %v, the registered commands are %v", got, want)
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), "crw", []string{"flow", "status"}, &out, &errOut); code != 2 {
		t.Errorf("crw flow status: %d, want 2", code)
	}
	if got := flowUsageWords(errOut.String()); !slices.Equal(got, want) {
		t.Errorf("crw flow status names %v in its usage, the registered commands are %v", got, want)
	}
	choices := regexp.MustCompile(`invalid choice: "status" \(choose from ([^)]*)\)`).FindStringSubmatch(errOut.String())
	if choices == nil {
		t.Fatalf("crw flow status gives no choices: %q", errOut.String())
	}
	var listed []string
	for _, c := range strings.Split(choices[1], ", ") {
		listed = append(listed, strings.Trim(c, "'"))
	}
	slices.Sort(listed)
	if !slices.Equal(listed, want) {
		t.Errorf("crw flow status offers %v, the registered commands are %v", listed, want)
	}
}

// TestFlowHelpIsTheDispatchTable: the commands `crw flow --help` names are the words the flow mode dispatches
// (flowCommandWords: internal/flow/cli, internal/flow/shadow, lint and the engine), so a command added to a table
// without a usage line, or a usage line for a command nobody serves, fails here. `crw flow next` and `crw flow
// dispatched` (CRW-1348) are in both since that change landed.
func TestFlowHelpIsTheDispatchTable(t *testing.T) {
	var out, errOut strings.Builder
	if code := run(context.Background(), "crw", []string{"flow", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("crw flow --help: %d %q", code, errOut.String())
	}
	want := slices.Clone(flowCommandWords())
	slices.Sort(want)
	if got := flowUsageWords(out.String()); !slices.Equal(got, want) {
		t.Errorf("crw flow --help names %v, the dispatch tables register %v", got, want)
	}
}
