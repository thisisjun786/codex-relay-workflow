//go:build dev

// Package stopevents is `crw-dev stop-events`: whether every Stop event recorded under the given
// journal roots was accepted exactly once. It is the per-event reading CRW-212 put in place of
// the per-(session, turn) count, ported by property from scripts/stop_events.py and
// completion.stop_events: a turn can end several times and each Stop is its own event, while two
// registrations answering one Stop are one event handled twice. Records are judged against the
// Go hook's own record vocabulary and EventKey (internal/relay/hook), so the writer and this
// reader cannot disagree about a name.
//
// Give every journal root the host's registrations write to, and the host's Codex home. The
// registrations of one host meet in one file under its Codex home before they claim in their own
// roots, so an event is accepted in one root. The host ledgers the claims name are read without
// being asked for; --codex-home adds a host whose ledger no claim names yet. Every record of an
// event the window reaches is checked whatever its own time. TRUE means every invocation in the
// window was judged. It prints the reading as JSON and exits 0 TRUE, 1 FALSE, 3 UNREADABLE and 2
// on a usage error.
package stopevents

import (
	"fmt"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

const usage = "usage: crw-dev stop-events [-h] --journal-root JOURNAL_ROOT [--since SINCE] [--until UNTIL] [--session SESSION] [--turn TURN] [--codex-home CODEX_HOME]"

const help = `Whether every Stop event recorded under the given journal roots was accepted exactly once.

  --journal-root JOURNAL_ROOT  a journal root a registration writes to; repeat for each
  --since SINCE                records at or after this UTC time, YYYY-MM-DDTHH:MM:SSZ
  --until UNTIL                records before this UTC time, YYYY-MM-DDTHH:MM:SSZ
  --session SESSION            only this session
  --turn TURN                  only this turn
  --codex-home CODEX_HOME      a Codex home whose host ledger is read as well; repeat for each

Prints the reading as JSON. Exit 0 TRUE, 1 FALSE, 3 UNREADABLE; 2 is a usage error.`

// Exit is the status each verdict exits with.
var Exit = map[string]int{verdictTrue: 0, verdictFalse: 1, verdictUnreadable: 3}

// WindowBound is a --since or --until bound, which must be a time in the records' own format:
// records are compared with bounds as strings, which orders them only in whole UTC seconds.
func WindowBound(value string) error {
	if !stamp(value) {
		return fmt.Errorf("a window bound is a UTC time in the records' own format, YYYY-MM-DDTHH:MM:SSZ, not %q", value)
	}
	return nil
}

// parser is stop_events.py's argparse parser, so the command takes what that parser takes: a
// unique prefix of a flag, a value that looks like a negative number, and help wherever -h is.
var parser = argparse.Spec{Actions: []argparse.Action{
	{Flags: []string{"-h", "--help"}, Kind: "_HelpAction"},
	{Flags: []string{"--journal-root"}, Kind: "_AppendAction", Required: true},
	{Flags: []string{"--since"}, Type: "window_bound", Check: func(v string) bool { return WindowBound(v) == nil }},
	{Flags: []string{"--until"}, Type: "window_bound", Check: func(v string) bool { return WindowBound(v) == nil }},
	{Flags: []string{"--session"}},
	{Flags: []string{"--turn"}},
	{Flags: []string{"--codex-home"}, Kind: "_AppendAction"},
}}

// Run is `crw-dev stop-events`.
func Run(args []string, stdout, stderr io.Writer) int {
	parsed := argparse.ParseSpec(parser, args)
	if parsed.Help {
		fmt.Fprintln(stdout, usage+"\n\n"+help)
		return 0
	}
	if parsed.Message != "" {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "crw-dev stop-events: error: "+parsed.Message)
		return 2
	}
	var window Window
	for name, into := range map[string]**string{"since": &window.Since, "until": &window.Until, "session": &window.Session, "turn": &window.Turn} {
		if values, given := parsed.Values[name]; given {
			*into = &values[0]
		}
	}
	reading := Read(parsed.Values["journal-root"], window, parsed.Values["codex-home"])
	fmt.Fprintln(stdout, pyjson.Dumps(reading.Answer(), pyjson.Options{Indent: 2, SortKeys: true}))
	return Exit[reading.Verdict()]
}
