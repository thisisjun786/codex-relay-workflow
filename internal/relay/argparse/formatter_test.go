package argparse

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// formatted is one command's usage and help as the formatter prints them.
type formatted struct {
	Help  string `json:"help"`
	Usage string `json:"usage"`
}

// Compare shipped CLI copy (including wrapping) against the golden for every spec at several
// terminal widths, so adding a command cannot quietly reintroduce an 80-column table. The goldens
// began as the bytes Python's argparse formatter printed for the same parser (the root and its
// direct subcommands; the service subcommands' goldens are Go's own).
func Test24FormatterAllSpecsPythonBytes(t *testing.T) {
	for _, width := range []string{"40", "80", "120", "200", ""} {
		t.Run(width, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			if width == "" {
				if err := os.Unsetenv("COLUMNS"); err != nil {
					t.Fatal(err)
				}
			}
			got := map[string]formatted{}
			for name := range Specs {
				got[name] = formatted{Help: Help("codex-session-relay", name), Usage: Usage("codex-session-relay", name)}
			}
			golden.CheckJSON(t, "formatted", got)
		})
	}
}
