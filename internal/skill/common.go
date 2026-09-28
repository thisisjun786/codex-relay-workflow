package skill

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"

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
func jsonEqual(a, b any) bool { return reflect.DeepEqual(normalizeJSON(a), normalizeJSON(b)) }
func normalizeJSON(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return "'" + strings.ReplaceAll(strings.ReplaceAll(x, "\\", "\\\\"), "'", "\\'") + "'"
	default:
		raw, _ := json.Marshal(x)
		s := string(raw)
		s = strings.ReplaceAll(s, "true", "True")
		s = strings.ReplaceAll(s, "false", "False")
		s = strings.ReplaceAll(s, "null", "None")
		return s
	}
}
func readFileOrStdin(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}
