//go:build dev

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCommandTreeDispatches(t *testing.T) {
	for _, row := range []struct {
		args         []string
		code         int
		stdout, errs string
	}{
		{nil, 2, "", "the following arguments are required: command"},
		{[]string{"nope"}, 2, "", `invalid command "nope"`},
		{[]string{"--help"}, 0, "usage: crw-dev {ci,cxc,skills,stop-events,trial-ledger}", ""},
		{[]string{"ci"}, 2, "", "crw-dev ci: error: the following arguments are required: check"},
		{[]string{"ci", "--help"}, 0, "usage: crw-dev ci {contracts,operations,plugin,validate}", ""},
		{[]string{"ci", "nope"}, 2, "", `invalid choice: "nope"`},
		{[]string{"ci", "validate", "--bogus"}, 2, "", "flag provided but not defined: -bogus"},
		{[]string{"skills"}, 2, "", "crw-dev skills: error: the following arguments are required: command"},
		{[]string{"skills", "--help"}, 0, "usage: crw-dev skills {link}", ""},
		{[]string{"skills", "link", "--bogus"}, 2, "", "unrecognized arguments: --bogus"},
		{[]string{"stop-events"}, 2, "", "the following arguments are required: --journal-root"},
		{[]string{"stop-events", "--help"}, 0, "usage: crw-dev stop-events", ""},
		{[]string{"stop-events", "--journal-root", "/x", "--since", "2026-09-23T11:58:17.500Z"}, 2, "", `argument --since: invalid window_bound value: "2026-09-23T11:58:17.500Z"`},
		{[]string{"stop-events", "--journal-root"}, 2, "", "argument --journal-root: expected one argument"},
		{[]string{"trial-ledger"}, 2, "", "the following arguments are required: --start"},
		{[]string{"trial-ledger", "--help"}, 0, "usage: crw-dev trial-ledger", ""},
		{[]string{"trial-ledger", "--start", "x", "--bogus"}, 2, "", "unrecognized arguments: --bogus"},
		// Both read their lines with the relay's parser (decision R3C-1): a flag is spelled in
		// full, a value that looks like a negative number is a value, and help is asked for
		// wherever -h stands.
		{[]string{"stop-events", "--bogus", "-h"}, 0, "usage: crw-dev stop-events", ""},
		{[]string{"stop-events", "--h"}, 2, "", "unrecognized arguments: --h"},
		{[]string{"stop-events", "--jour", "/x"}, 2, "", "unrecognized arguments: --jour /x"},
		{[]string{"stop-events", "--journal-root", "/nonexistent-crw-dev", "--session", "-1", "--turn", "-5", "--until", "bad"}, 2, "", `argument --until: invalid window_bound value: "bad"`},
		{[]string{"stop-events", "--journal-root=/x", "--since=-1"}, 2, "", `argument --since: invalid window_bound value: "-1"`},
		{[]string{"stop-events", "--journal-root", "/nonexistent-crw-dev", "--", "extra"}, 2, "", "unrecognized arguments: -- extra"},
		{[]string{"stop-events", "--journal-root", "/nonexistent-crw-dev", "--session", "-x"}, 2, "", "argument --session: expected one argument"},
		{[]string{"stop-events", "--journal-root", "/nonexistent-crw-dev", "--codex-home", "/nonexistent-crw-dev-home", "--session", "-1", "--turn", "-5"}, 3, `"session": "-1"`, ""},
		{[]string{"stop-events", "--journal-root", "/nonexistent-crw-dev", "--session", "-1", "--turn", "-5"}, 3, `"turn": "-5"`, ""},
		{[]string{"stop-events", "--journal-root", "-1", "--session", "-1 x"}, 3, `"session": "-1 x"`, ""},
		{[]string{"trial-ledger", "--bogus", "-h"}, 0, "usage: crw-dev trial-ledger", ""},
		{[]string{"trial-ledger", "--start"}, 2, "", "argument --start: expected one argument"},
		{[]string{"trial-ledger", "--st", "x"}, 2, "", "unrecognized arguments: --st x"},
		{[]string{"trial-ledger", "--start", "-1"}, 2, `"refused": "--start must be an absolute path"`, ""},
		{[]string{"trial-ledger", "--start=-1"}, 2, `"value": "-1"`, ""},
	} {
		var stdout, stderr bytes.Buffer
		code := run(row.args, &stdout, &stderr)
		if code != row.code || !strings.Contains(stdout.String(), row.stdout) || !strings.Contains(stderr.String(), row.errs) {
			t.Errorf("%q: exit %d stdout %q stderr %q", row.args, code, stdout.String(), stderr.String())
		}
	}
}
