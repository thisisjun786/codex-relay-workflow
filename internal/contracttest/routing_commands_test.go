package contracttest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// No existing cli-shape or hook fixture names a routing command. This boundary suite therefore
// replays the help and error cases the Python parser derived for every routing command
// (testdata/fixtures/routing-argv.json) and holds each exit status and output to the golden
// (first taken as what the Python parser printed).
func TestRoutingCommandsPythonArgparseBytes(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	var cases []struct {
		Command string
		Args    []string
		Width   *string
	}
	if err := json.Unmarshal(golden.Fixture(t, "routing-argv.json"), &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no routing case")
	}
	for _, tc := range cases {
		cmd := exec.Command(binary, append([]string{"relay", "--state", home + "/relay", tc.Command}, tc.Args...)...)
		env := []string{}
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "COLUMNS=") {
				env = append(env, v)
			}
		}
		width := "unset"
		if tc.Width != nil {
			env = append(env, "COLUMNS="+*tc.Width)
			width = *tc.Width
		}
		cmd.Env = env
		answer, err := runProcess(cmd)
		if err != nil {
			t.Fatal(err)
		}
		checkProcess(t, fmt.Sprintf("%s %q COLUMNS=%s", tc.Command, tc.Args, width), answer, golden.Substitute(home, "<HOME>"), golden.Substitute(root, "<ROOT>"))
	}
}
