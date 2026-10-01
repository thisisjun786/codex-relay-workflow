package argparse

import (
	"math/big"
	"slices"
	"strings"
	"testing"
)

// The parser's contract: which lines it reads, what it keeps of them, and the kind of refusal
// for a line it cannot read (exit 2 at the caller). The wording of a refusal is the parser's
// own; the tests hold the flag or value it names.
func TestParseReadsWhatTheSpecDeclares(t *testing.T) {
	spec := Spec{Actions: []Action{help,
		{Flags: []string{"--name"}, Required: true},
		{Flags: []string{"--count"}, Type: "int"},
		{Flags: []string{"--seconds"}, Type: "float"},
		{Flags: []string{"--mode"}, Choices: []string{"observe", "hold"}},
		{Flags: []string{"--tag"}, Kind: KindAppend},
		{Flags: []string{"--dry-run"}, Kind: KindTrue},
		{Flags: []string{"--not-ready"}, Kind: KindFalse},
		{Flags: []string{"--left"}},
		{Flags: []string{"--right"}},
	}, Groups: []Group{{Actions: []int{8, 9}}}}
	for _, c := range []struct {
		argv []string
		want map[string][]string
	}{
		{[]string{"--name", "n"}, map[string][]string{"name": {"n"}}},
		{[]string{"--name=n=m"}, map[string][]string{"name": {"n=m"}}},
		{[]string{"--name", "a", "--name", "b"}, map[string][]string{"name": {"b"}}},
		{[]string{"--name", "-1"}, map[string][]string{"name": {"-1"}}},
		{[]string{"--name", "-.5"}, map[string][]string{"name": {"-.5"}}},
		{[]string{"--name", "- a list item"}, map[string][]string{"name": {"- a list item"}}},
		{[]string{"--name", "-"}, map[string][]string{"name": {"-"}}},
		{[]string{"--name", ""}, map[string][]string{"name": {""}}},
		{[]string{"--name=-x"}, map[string][]string{"name": {"-x"}}},
		{[]string{"--name", "n", "--count", "+7"}, map[string][]string{"name": {"n"}, "count": {"7"}}},
		{[]string{"--name", "n", "--count=-9223372036854775808"}, map[string][]string{"name": {"n"}, "count": {"-9223372036854775808"}}},
		{[]string{"--name", "n", "--seconds", "1e3"}, map[string][]string{"name": {"n"}, "seconds": {"1000"}}},
		{[]string{"--name", "n", "--mode", "hold"}, map[string][]string{"name": {"n"}, "mode": {"hold"}}},
		{[]string{"--name", "n", "--tag", "a", "--tag=b"}, map[string][]string{"name": {"n"}, "tag": {"a", "b"}}},
		{[]string{"--name", "n", "--dry-run", "--not-ready"}, map[string][]string{"name": {"n"}, "dry-run": {"true"}, "not-ready": {"false"}}},
		{[]string{"--name", "n", "--left", "l"}, map[string][]string{"name": {"n"}, "left": {"l"}}},
	} {
		r := ParseSpec(spec, c.argv)
		if r.Message != "" || r.Help || len(r.Values) != len(c.want) {
			t.Errorf("%q: %+v", c.argv, r)
			continue
		}
		for key, values := range c.want {
			if !slices.Equal(r.Values[key], values) || !r.Given[key] {
				t.Errorf("%q: %s = %q, want %q", c.argv, key, r.Values[key], values)
			}
		}
	}
	r := ParseSpec(spec, []string{"--name", "n", "--count", "9223372036854775807", "--seconds", "-0.25"})
	if n, ok := r.Numbers["count"].(*big.Int); !ok || n.String() != "9223372036854775807" || r.Numbers["seconds"] != -0.25 {
		t.Errorf("numbers: %+v", r.Numbers)
	}
	for _, c := range []struct {
		argv      []string
		refusal   string
		mentioned string
	}{
		{nil, "the following arguments are required", "--name"},
		{[]string{"--nam", "n"}, "unrecognized arguments", "--nam"},
		{[]string{"--name", "n", "extra", "-x"}, "unrecognized arguments", "extra -x"},
		{[]string{"--name", "n", "--", "x"}, "unrecognized arguments", "-- x"},
		{[]string{"--name"}, "expected one argument", "--name"},
		{[]string{"--name", "--dry-run"}, "expected one argument", "--name"},
		{[]string{"--name", "-x"}, "expected one argument", "--name"},
		{[]string{"--name", "n", "--count", "1.5"}, "invalid int value", `"1.5"`},
		{[]string{"--name", "n", "--count", "9223372036854775808"}, "invalid int value", "9223372036854775808"},
		{[]string{"--name", "n", "--count", " 7"}, "invalid int value", `" 7"`},
		{[]string{"--name", "n", "--count", "1_0"}, "invalid int value", "1_0"},
		{[]string{"--name", "n", "--count", "٣"}, "invalid int value", "٣"},
		{[]string{"--name", "n", "--seconds", "1e400"}, "invalid float value", "1e400"},
		{[]string{"--name", "n", "--mode", "wait"}, "invalid choice", `"wait"`},
		{[]string{"--name", "n", "--dry-run=yes"}, "takes no value", "--dry-run"},
		{[]string{"--name", "n", "--left", "l", "--right", "r"}, "not allowed with", "--left"},
	} {
		r := ParseSpec(spec, c.argv)
		if r.Help || !strings.Contains(r.Message, c.refusal) || !strings.Contains(r.Message, c.mentioned) {
			t.Errorf("%q: %q, want %q naming %q", c.argv, r.Message, c.refusal, c.mentioned)
		}
	}
	// Help is asked for by -h or --help as a token of its own, wherever it stands among options
	// the parser does not know; -hx is not help.
	for _, argv := range [][]string{{"-h"}, {"--help"}, {"--bogus", "-h"}, {"--name", "n", "--help"}} {
		if r := ParseSpec(spec, argv); !r.Help {
			t.Errorf("%q is not help: %+v", argv, r)
		}
	}
	for _, argv := range [][]string{{"-hx"}, {"--he"}, {"--help=x"}, {"--name", "-h"}} {
		if r := ParseSpec(spec, argv); r.Help || r.Message == "" {
			t.Errorf("%q: %+v", argv, r)
		}
	}
	required := Spec{Actions: []Action{help, {Flags: []string{"--a"}}, {Flags: []string{"--b"}, Kind: KindTrue}}, Groups: []Group{{Required: true, Actions: []int{1, 2}}}}
	if r := ParseSpec(required, nil); !strings.Contains(r.Message, "one of the arguments --a --b is required") {
		t.Errorf("required group: %+v", r)
	}
	if r := ParseSpec(required, []string{"--b"}); r.Message != "" {
		t.Errorf("required group given: %+v", r)
	}
}

// The root parser reads the global options up to the first word, which names the command; the
// command's own options are the command's parser's to read.
func TestTheRootParserStopsAtTheCommand(t *testing.T) {
	r := Parse("", []string{"--state", "/s", "--kind-module", "a", "--kind-module=b", "--json", "doctor", "--issue", "x"})
	if r.Message != "" || !slices.Equal(r.Remaining, []string{"doctor", "--issue", "x"}) || r.Values["state"][0] != "/s" ||
		!slices.Equal(r.Values["kind-module"], []string{"a", "b"}) || !r.Given["json"] {
		t.Errorf("%+v", r)
	}
	for _, c := range []struct {
		argv    []string
		refusal string
	}{
		{nil, "the following arguments are required: command"},
		{[]string{"--state", "/s"}, "the following arguments are required: command"},
		{[]string{"--bogus", "doctor"}, "unrecognized arguments: --bogus"},
		{[]string{"--state"}, "argument --state: expected one argument"},
	} {
		if r := Parse("", c.argv); r.Message != c.refusal {
			t.Errorf("%q: %q, want %q", c.argv, r.Message, c.refusal)
		}
	}
	if r := Parse("service", []string{"start", "--deadline", "5"}); r.Message != "" || !slices.Equal(r.Remaining, []string{"start", "--deadline", "5"}) {
		t.Errorf("service: %+v", r)
	}
}

// "--" ends the options of a parser that names commands, which takes the next word as its
// command; a command's parser reads no words, so it refuses "--" with what follows it.
func TestTheEndOfOptionsMarker(t *testing.T) {
	for _, c := range []struct {
		command   string
		argv      []string
		remaining []string
		refusal   string
	}{
		{"", []string{"--", "doctor"}, []string{"doctor"}, ""},
		{"", []string{"--state", "/s", "--", "doctor", "--issue", "x"}, []string{"doctor", "--issue", "x"}, ""},
		{"", []string{"--"}, nil, "the following arguments are required: command"},
		{"service", []string{"--", "status"}, []string{"status"}, ""},
		{"", []string{"--", "--json"}, []string{"--json"}, ""},
		{"doctor", []string{"--", "x"}, nil, "unrecognized arguments: -- x"},
	} {
		r := Parse(c.command, c.argv)
		if r.Message != c.refusal || !slices.Equal(r.Remaining, c.remaining) {
			t.Errorf("%q %q: message %q remaining %q; want %q %q", c.command, c.argv, r.Message, r.Remaining, c.refusal, c.remaining)
		}
	}
}

// Every declared parser can be asked for help, which lists every option it reads, and its usage
// line names them too; every parser that names commands lists them.
func TestEveryParserListsItsOptions(t *testing.T) {
	if len(Specs) < 150 || len(Commands("")) < 140 || !slices.Equal(Commands("service"), []string{"service status", "service enable", "service disable", "service stop", "service declare", "service start", "service restart", "service run"}) {
		t.Fatalf("%d parsers, %d commands, service %q", len(Specs), len(Commands("")), Commands("service"))
	}
	for name, spec := range Specs {
		if spec.Actions[0].Kind != KindHelp {
			t.Errorf("%q: the first option is not help", name)
		}
		help, usage := Help("crw relay", name), Usage("crw relay", name)
		if !strings.HasPrefix(help, usage+"\n") || strings.Contains(usage, "\n") || !strings.HasPrefix(usage, strings.TrimSpace("usage: crw relay "+name)) {
			t.Errorf("%q: usage %q, help %q", name, usage, help)
		}
		for _, a := range spec.Actions {
			for _, flag := range a.Flags {
				if !strings.Contains(help, flag) || !strings.Contains(usage, a.Flags[0]) {
					t.Errorf("%q: help or usage does not name %s", name, flag)
				}
			}
			if a.Help != "" && !strings.Contains(help, a.Help) {
				t.Errorf("%q: help leaves out what %s does", name, a.Flags[0])
			}
		}
		if spec.Commands {
			for _, command := range Commands(name) {
				if !strings.Contains(help, "\n  "+strings.TrimPrefix(command, name+" ")) {
					t.Errorf("%q: help does not list %q", name, command)
				}
			}
		}
	}
}
