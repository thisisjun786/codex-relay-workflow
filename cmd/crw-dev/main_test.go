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
		{[]string{"--help"}, 0, "usage: crw-dev {ci}", ""},
		{[]string{"ci"}, 2, "", "crw-dev ci: error: the following arguments are required: check"},
		{[]string{"ci", "--help"}, 0, "usage: crw-dev ci {contracts,gate,operations,plugin,scope,validate}", ""},
		{[]string{"ci", "nope"}, 2, "", "invalid choice: 'nope'"},
		{[]string{"ci", "scope", "--bogus"}, 2, "", "unrecognized arguments: --bogus"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(row.args, &stdout, &stderr)
		if code != row.code || !strings.Contains(stdout.String(), row.stdout) || !strings.Contains(stderr.String(), row.errs) {
			t.Errorf("%q: exit %d stdout %q stderr %q", row.args, code, stdout.String(), stderr.String())
		}
	}
}
