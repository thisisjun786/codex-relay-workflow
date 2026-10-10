package cli

// CRW-1109 through the orchestrate CLI parser, one case per swept defect (docs/port-cxc/known-defects.md
// :529-:532). Red on dev: the inherited verbs parsed, a help token taken as an option's value turned the
// command into help, unknown and swallowed flags passed silently so a typo sent the work to the input cwd,
// and a repeated --session was chosen one way for the diagnostic and another way for the command.

import (
	"strings"
	"testing"
)

// :529 - only the documented verbs are verbs.
func TestOrchestrateArgsDocumentedVerbsOnly(t *testing.T) {
	for _, verb := range []string{"constructor", "CONSTRUCTOR", "__proto__", "__PROTO__"} {
		if p := ParseOrchestrateCliArgs([]string{verb, "--session", "s1"}, "/ws"); p.Error == nil || !strings.Contains(p.Error.Error, "unknown orchestrate verb") {
			t.Errorf("%s: %+v", verb, p)
		}
	}
}

// :530 - help is decided after the option values: a session named help is a session; help in an option
// position, and the top-level --help and -h, are still help.
func TestOrchestrateArgsHelpAfterTheValues(t *testing.T) {
	if p := ParseOrchestrateCliArgs([]string{"status", "--session", "help"}, "/ws"); p.Args == nil || p.Args.Session == nil || *p.Args.Session != "help" {
		t.Errorf("a session named help: %+v", p)
	}
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"help"}, {"status", "--help"}, {"A", "--cwd", "x", "--help"}} {
		if ParseOrchestrateCliArgs(argv, "/ws").Help == nil {
			t.Errorf("%q is not help", argv)
		}
	}
}

// :531 - the options are parsed strictly: an unknown option, a stray argument, a missing or swallowed value
// is refused instead of being ignored, and the --flag=value form is read. The calls the skills use parse.
func TestOrchestrateArgsStrictOptions(t *testing.T) {
	for _, argv := range [][]string{
		{"P", "--sesion", "s1"},
		{"P", "--session", "s1", "--cdw", "/elsewhere"},
		{"P", "--session", "s1", "stray"},
		{"P", "--session"},
		{"P", "--session", "s1", "--cwd"},
		{"P", "--session", "--cwd", "/elsewhere"},
		{"P", "--session", "s1", "--json=true"},
		// CRW-1109 fix round: --attest swallows no option either.
		{"reset", "--session", "s1", "--attest", "--cwd=/elsewhere"},
		{"status", "--session", "s1", "--attest", "--cwd", "/elsewhere"},
		{"P", "--session", "s1", "--attest", "--session=s2"},
	} {
		if p := ParseOrchestrateCliArgs(argv, "/ws"); p.Error == nil {
			t.Errorf("%q was accepted: %+v", argv, p.Args)
		}
	}
	if p := ParseOrchestrateCliArgs([]string{"P", "--session=s1", "--cwd=/elsewhere"}, "/ws"); p.Args == nil || *p.Args.Session != "s1" || p.Args.Cwd != "/elsewhere" {
		t.Errorf("the = form: %+v", p)
	}
	for _, argv := range [][]string{
		{"P", "--session", "<id>"},
		{"I", "--session", "<id>"},
		{"status", "--session", "<id>"},
		{"status", "--session", "<id>", "--json"},
		{"reset"},
		{"P", "--attest-file", "<path>"},
		{"A", "--session", "<id>", "--attest-file", ".crw/attest.json"},
		{"A", "--session", "<id>", "--attest", `{"from":"P","to":"A","did":"wrote and audited the plan","planUnit":"devlog/_plan/260714_slug","workPhaseId":"wp1"}`},
		{"D", "--session", "<id>", "--cwd", "/ws", "--attest", `{"from":"C","to":"D","did":"verified","checkOutput":"tests passed","exitCode":0}`},
	} {
		if p := ParseOrchestrateCliArgs(argv, "/ws"); p.Args == nil || p.Error != nil {
			t.Errorf("the skill call %q does not parse: %+v", argv, p.Error)
		}
	}
}

// :532 - a --session or --cwd repeated with another value is refused before anything is written, and the
// unknown-verb diagnostic selects by the same rule (a conflicting repeat names no session); a repeat of the
// same value is fine.
func TestOrchestrateArgsConflictingRepeats(t *testing.T) {
	for _, argv := range [][]string{
		{"A", "--session", "one", "--session", "two"},
		{"A", "--session", "s1", "--cwd", "first", "--cwd", "second"},
	} {
		if p := ParseOrchestrateCliArgs(argv, "/ws"); p.Error == nil || !strings.Contains(p.Error.Error, "more than once with different values") {
			t.Errorf("%q: %+v", argv, p)
		}
	}
	if p := ParseOrchestrateCliArgs([]string{"A", "--session", "s1", "--session", "s1"}, "/ws"); p.Args == nil || *p.Args.Session != "s1" {
		t.Errorf("a repeat of the same value: %+v", p)
	}
	p := ParseOrchestrateCliArgs([]string{"wat", "--session", "first", "--session", "last", "--cwd", "one", "--cwd", "two"}, "/ws")
	if p.Error == nil || p.Error.Session != nil || p.Error.Cwd != "/ws" {
		t.Errorf("the unknown-verb diagnostic of conflicting repeats: %+v", p.Error)
	}
	p = ParseOrchestrateCliArgs([]string{"wat", "--session", "only", "--cwd", "dir"}, "/ws")
	if p.Error == nil || p.Error.Session == nil || *p.Error.Session != "only" || p.Error.Cwd != "dir" {
		t.Errorf("the unknown-verb diagnostic of single options: %+v", p.Error)
	}
}
