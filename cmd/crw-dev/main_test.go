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
		{[]string{"--help"}, 0, "usage: crw-dev {ci,skills,stop-events,trial-ledger}", ""},
		{[]string{"ci"}, 2, "", "crw-dev ci: error: the following arguments are required: check"},
		{[]string{"ci", "--help"}, 0, "usage: crw-dev ci {contracts,gate,operations,plugin,scope,validate}", ""},
		{[]string{"ci", "nope"}, 2, "", "invalid choice: 'nope'"},
		{[]string{"ci", "scope", "--bogus"}, 2, "", "unrecognized arguments: --bogus"},
		{[]string{"skills"}, 2, "", "crw-dev skills: error: the following arguments are required: command"},
		{[]string{"skills", "--help"}, 0, "usage: crw-dev skills {link}", ""},
		{[]string{"skills", "link", "--bogus"}, 2, "", "unrecognized arguments: --bogus"},
		{[]string{"stop-events"}, 2, "", "the following arguments are required: --journal-root"},
		{[]string{"stop-events", "--help"}, 0, "usage: crw-dev stop-events", ""},
		{[]string{"stop-events", "--journal-root", "/x", "--since", "2026-09-23T11:58:17.500Z"}, 2, "", "argument --since: invalid window_bound value: '2026-09-23T11:58:17.500Z'"},
		{[]string{"stop-events", "--journal-root"}, 2, "", "argument --journal-root: expected one argument"},
		{[]string{"trial-ledger"}, 2, "", "the following arguments are required: --start"},
		{[]string{"trial-ledger", "--help"}, 0, "usage: crw-dev trial-ledger", ""},
		{[]string{"trial-ledger", "--start", "x", "--bogus"}, 2, "", "unrecognized arguments: --bogus"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(row.args, &stdout, &stderr)
		if code != row.code || !strings.Contains(stdout.String(), row.stdout) || !strings.Contains(stderr.String(), row.errs) {
			t.Errorf("%q: exit %d stdout %q stderr %q", row.args, code, stdout.String(), stderr.String())
		}
	}
}
