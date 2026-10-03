package testsupport

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"testing"
)

// Relay runs the built binary as an operator would, one process per command: crw relay --state <state> <args>.
// It returns what the command printed on stdout and its exit code (0 an answer, 2 a refusal, 3 a host problem,
// 4 a usage error); a process that cannot be started fails the test.
func Relay(t testing.TB, state string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(CRW(t), append([]string{"relay", "--state", state}, args...)...)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return out.String(), code
}

// ParseJSON is the JSON object a relay command printed; output that encoding/json rejects as one fails the test and is shown.
func ParseJSON(t testing.TB, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return m
}
