package contracttest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// No existing cli-shape or hook fixture names a routing command. This boundary suite
// therefore derives their help/error cases from the live parser, without a skip path.
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
	script := filepath.Join(root, "internal/relay/routing/testdata/cli_capture.py")
	cmd := exec.Command("uv", "run", "--no-sync", "--no-project", "python3", script, "argv", home)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "CODEX_HOME="+home+"/codex", "UV_PYTHON_DOWNLOADS=never")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("Python %v %s", err, stderr.String())
	}
	var cases []struct {
		Command        string
		Args           []string
		Prog           string
		Width          *string
		Code           int
		Stdout, Stderr string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		if tc.Prog != "crw relay" {
			continue
		}
		cmd := exec.Command(binary, append([]string{"relay", "--state", home + "/relay", tc.Command}, tc.Args...)...)
		env := []string{}
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "COLUMNS=") {
				env = append(env, v)
			}
		}
		if tc.Width != nil {
			env = append(env, "COLUMNS="+*tc.Width)
		}
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				code = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != tc.Code || stdout.String() != tc.Stdout || stderr.String() != tc.Stderr {
			t.Fatalf("%s %v differs exit Python=%d Go=%d\nPython=%q\nGo=%q", tc.Command, tc.Args, tc.Code, code, tc.Stderr, stderr.String())
		}
	}
}
