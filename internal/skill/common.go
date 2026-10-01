package skill

import (
	"io"
	"io/fs"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/plugins"
)

var bundledSkillFiles fs.FS = plugins.SkillFiles

func defaultFixture(name string) string {
	return "crw/skills/crw-run/scripts/fixtures/" + name
}
func defaultContract(name string) string {
	return "crw/skills/crw-run/references/" + name
}

func readFileOrStdin(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}
