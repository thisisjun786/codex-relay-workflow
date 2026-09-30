package cli_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

func TestKindModuleNestedImportErrorMatchesPython(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	args := []string{"--state", filepath.Join(home, "relay"), "--json", "--kind-module", "codex_session_relay.not_real", "status"}
	python := oracleRun(t, "codex-session-relay "+oracleLabel(args...), func() (run, error) {
		cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
		var pyOut, pyErr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &pyOut, &pyErr
		err := cmd.Run()
		pyCode := 0
		if err != nil {
			exit, ok := err.(*exec.ExitError)
			if !ok {
				return run{}, err
			}
			pyCode = exit.ExitCode()
		}
		return run{pyCode, pyOut.String(), pyErr.String()}, nil
	}, placeholders(append([]string{home}, args...)...)...)
	pyCode, pyOut, pyErr := python.code, python.stdout, python.stderr
	var goOut, goErr bytes.Buffer
	goCode := cli.Execute(context.Background(), args, &goOut, &goErr)
	if goCode != pyCode || goOut.String() != pyOut || goErr.String() != pyErr {
		t.Fatalf("Python: code=%d stdout=%q stderr=%q\nGo: code=%d stdout=%q stderr=%q", pyCode, pyOut, pyErr, goCode, goOut.String(), goErr.String())
	}
}
