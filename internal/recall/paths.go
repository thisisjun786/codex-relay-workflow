// CXC v0.2.40 recall/src/paths.ts ignores CODEX_SQLITE_HOME.
package recall

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

func codexHome(env ...host.LookupEnv) (string, error) {
	lookup := host.LookupEnv(os.LookupEnv)
	if len(env) != 0 {
		lookup = env[0]
	}
	if value, _ := lookup("CODEX_HOME"); text.Trim(value) != "" {
		return filepath.Abs(source.DecodeUTF8([]byte(value)))
	}
	home, err := host.Home(os.LookupEnv)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func sessionsDir(home string) string { return filepath.Join(home, "sessions") }
func memoriesDir(home string) string { return filepath.Join(home, "memories") }

// Missing home is null; sorted names break Number ties, including non-files.
func latestVersionedDb(home, prefix string) (string, error) {
	if _, err := os.Stat(home); err != nil {
		return "", nil
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", err
	}
	best, bestNumber := "", float64(0)
	for _, entry := range entries {
		name := source.DecodeUTF8([]byte(entry.Name()))
		digits, found := strings.CutPrefix(name, prefix+"_")
		digits, suffix := strings.CutSuffix(digits, ".sqlite")
		if !found || !suffix || digits == "" || strings.Trim(digits, "0123456789") != "" {
			continue
		}
		number, _ := strconv.ParseFloat(digits, 64)
		if best == "" || number > bestNumber {
			best, bestNumber = name, number
		}
	}
	if best == "" {
		return "", nil
	}
	return filepath.Join(home, best), nil
}
func stateDbPath(home string) (string, error)    { return latestVersionedDb(home, "state") }
func memoriesDbPath(home string) (string, error) { return latestVersionedDb(home, "memories") }
