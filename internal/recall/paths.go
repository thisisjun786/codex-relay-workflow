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

// RecallPhysicalAbs resolves path as Node's path.resolve does: process.cwd() is the
// physical directory, while Go's os.Getwd honors a logical $PWD, so a relative path
// joins the symlink-resolved working directory. Only that base is resolved; path
// itself may not exist and keeps its own symlinks. The result of a failed lookup is "".
func RecallPhysicalAbs(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if cwd, err = filepath.EvalSymlinks(cwd); err != nil {
		return "", err
	}
	return filepath.Join(cwd, path), nil
}

func codexHome(env ...host.LookupEnv) (string, error) {
	lookup := host.LookupEnv(os.LookupEnv)
	if len(env) != 0 {
		lookup = env[0]
	}
	if value, _ := lookup("CODEX_HOME"); text.Trim(value) != "" {
		return RecallPhysicalAbs(source.DecodeUTF8([]byte(value)))
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
