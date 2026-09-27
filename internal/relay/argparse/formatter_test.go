package argparse

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Compare shipped CLI copy (including wrapping) against the live formatter for
// every spec, so adding a command cannot quietly reintroduce an 80-column table.
func Test24FormatterAllSpecsPythonBytes(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	script := `import argparse,json,os
from codex_session_relay.cli import build_parser
p=build_parser(); children=next(a.choices for a in p._actions if isinstance(a,argparse._SubParsersAction))
print(json.dumps({n:dict(help=c.format_help(),usage=c.format_usage().rstrip()) for n,c in {'':p,**children}.items()}))`
	for _, width := range []string{"40", "80", "120", "200", ""} {
		t.Run(width, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			if width == "" {
				if err := os.Unsetenv("COLUMNS"); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script)
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("oracle: %v %s", err, raw)
			}
			var expected map[string]struct{ Help, Usage string }
			if err = json.Unmarshal(raw, &expected); err != nil {
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
