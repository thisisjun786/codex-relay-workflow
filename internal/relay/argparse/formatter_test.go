package argparse

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// Compare shipped CLI copy (including wrapping) against the live formatter for
// every spec, so adding a command cannot quietly reintroduce an 80-column table. The
// formatter's answer is recorded (pyoracle).
func Test24FormatterAllSpecsPythonBytes(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	script := `import argparse,json,os
from codex_session_relay.cli import build_parser
p=build_parser(); children=next(a.choices for a in p._actions if isinstance(a,argparse._SubParsersAction))
print(json.dumps({n:dict(help=c.format_help(),usage=c.format_usage().rstrip()) for n,c in {'':p,**children}.items()}))`
	// Every width is asked in this test, not its subtests, so the answers share one recording,
	// which is then large enough to be kept compressed.
	widths := []string{"40", "80", "120", "200", ""}
	answers := map[string][]byte{}
	for _, width := range widths {
		answers[width] = pyoracle.Answer(t, "COLUMNS="+width, func() ([]byte, error) {
			cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "COLUMNS=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			if width != "" {
				cmd.Env = append(cmd.Env, "COLUMNS="+width)
			}
			raw, err := cmd.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("oracle: %v %s", err, raw)
			}
			return raw, nil
		})
	}
	for _, width := range widths {
		t.Run(width, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			if width == "" {
				if err := os.Unsetenv("COLUMNS"); err != nil {
					t.Fatal(err)
				}
			}
			var expected map[string]struct{ Help, Usage string }
			if err := json.Unmarshal(answers[width], &expected); err != nil {
				t.Fatal(err)
			}
			for name, want := range expected {
				if got := Usage("codex-session-relay", name); got != want.Usage {
					t.Errorf("%s usage byte diff\nGo=%q\nPython=%q", name, got, want.Usage)
				}
				if got := Help("codex-session-relay", name); got != want.Help {
					t.Errorf("%s help byte diff\nGo=%q\nPython=%q", name, got, want.Help)
				}
			}
		})
	}
}
