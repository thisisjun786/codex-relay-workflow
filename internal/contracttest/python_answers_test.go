package contracttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// processAnswer is what one process answered: its exit status and output bytes.
type processAnswer struct {
	exit           int
	stdout, stderr []byte
}

// storedProcess is processAnswer as a recording holds it: output that is UTF-8 is kept as text,
// so a run-specific path in it can be substituted; other output is kept as base64.
type storedProcess struct {
	Exit      int     `json:"exit"`
	Stdout    *string `json:"stdout,omitempty"`
	StdoutB64 []byte  `json:"stdoutBase64,omitempty"`
	Stderr    *string `json:"stderr,omitempty"`
	StderrB64 []byte  `json:"stderrBase64,omitempty"`
}

func storedText(raw []byte) (*string, []byte) {
	if utf8.Valid(raw) {
		text := string(raw)
		return &text, nil
	}
	return nil, raw
}

func loadedText(text *string, raw []byte) []byte {
	if text != nil {
		return []byte(*text)
	}
	return raw
}

// pythonProcess returns what the Python implementation's process answered under key: capture runs
// it live in record and check mode, and the recording answers otherwise (internal/testsupport/pyoracle).
func pythonProcess(t *testing.T, key string, capture func() (processAnswer, error), opts ...pyoracle.Option) processAnswer {
	t.Helper()
	raw := pyoracle.Answer(t, key, func() ([]byte, error) {
		answer, err := capture()
		if err != nil {
			return nil, err
		}
		var stored storedProcess
		stored.Exit = answer.exit
		stored.Stdout, stored.StdoutB64 = storedText(answer.stdout)
		stored.Stderr, stored.StderrB64 = storedText(answer.stderr)
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(stored); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}, opts...)
	var stored storedProcess
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("recorded Python process %q: %v", key, err)
	}
	return processAnswer{exit: stored.Exit, stdout: loadedText(stored.Stdout, stored.StdoutB64), stderr: loadedText(stored.Stderr, stored.StderrB64)}
}

// runProcess runs cmd to completion and returns its answer; only a failure to start is an error.
func runProcess(cmd *exec.Cmd) (processAnswer, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	answer := processAnswer{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return answer, err
		}
		answer.exit = exit.ExitCode()
	}
	answer.stdout, answer.stderr = stdout.Bytes(), stderr.Bytes()
	return answer, nil
}
