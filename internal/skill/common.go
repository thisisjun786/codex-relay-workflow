package skill

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/plugins"
)

var bundledSkillFiles fs.FS = plugins.SkillFiles

func defaultFixture(name string) string {
	return "crw/skills/crw-run/scripts/fixtures/" + name
}
func defaultContract(name string) string {
	return "crw/skills/crw-run/references/" + name
}
func argparseMissing(w io.Writer, prog, what string) int {
	fmt.Fprintf(w, "usage: %s [-h] ...\n%s: error: the following arguments are required: %s\n", prog, prog, what)
	return 2
}
func argparseValue(w io.Writer, prog, option string) int {
	fmt.Fprintf(w, "usage: %s [-h] ...\n%s: error: argument %s: expected one argument\n", prog, prog, option)
	return 2
}
func invalidChoice(w io.Writer, prog, what, got string, choices ...string) int {
	quoted := make([]string, len(choices))
	for i, s := range choices {
		quoted[i] = "'" + s + "'"
	}
	fmt.Fprintf(w, "%s: error: argument %s: invalid choice: %q (choose from %s)\n", prog, what, got, strings.Join(quoted, ", "))
	return 2
}
func invalidOption(w io.Writer, prog, opt string) int {
	fmt.Fprintf(w, "%s: error: unrecognized arguments: %s\n", prog, opt)
	return 2
}

// argvRepr is repr() of a command-line argument as sys.argv holds it: os.fsdecode makes each
// byte outside a well-formed UTF-8 sequence, the three of an encoded surrogate included, the
// lone surrogate U+DC00+byte, and repr() escapes it.
func argvRepr(arg string) string { return evidence.StrRepr(store.FSDecode(arg)) }

// stderrText is text as Python's sys.stderr writes it, which always uses
// errors="backslashreplace": a lone surrogate, held as WTF-8, is written as its \uXXXX escape
// and everything else as it is.
func stderrText(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := settings.CodePoint(text, i)
		if r >= 0xd800 && r <= 0xdfff {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}

func readFileOrStdin(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}
