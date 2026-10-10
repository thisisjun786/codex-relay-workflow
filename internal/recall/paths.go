package recall

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// RecallPhysicalAbs resolves path as Node's path.resolve does: process.cwd() is the
// kernel's getcwd, which names the physical directory and needs no permission on any
// ancestor, while Go's os.Getwd honors a logical $PWD. A relative path therefore joins
// syscall.Getwd; only that base is physical, and path itself may not exist and keeps
// its own symlinks. The result of a failed lookup is "".
func RecallPhysicalAbs(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	cwd, err := syscall.Getwd()
	if err != nil {
		return "", os.NewSyscallError("getwd", err)
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
	home, err := recallHome(lookup)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// recallHome is the user's home directory for a default location. An empty HOME names no directory: the
// default would resolve against the working directory and create state in the workspace, so it is refused
// and no other home is chosen in its place.
func recallHome(env host.LookupEnv) (string, error) {
	home, err := host.Home(env)
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("cannot resolve the home directory: HOME is empty")
	}
	return home, nil
}

func sessionsDir(home string) string { return filepath.Join(home, "sessions") }
func memoriesDir(home string) string { return filepath.Join(home, "memories") }

// physicalHome names the directory the system lists for home. Joining names onto a path that has a link
// followed by `..` would name the lexical parent instead, so the home is resolved once, before it is
// listed and before names are joined to it. A home whose cleaned spelling is already that directory
// keeps its spelling.
func physicalHome(home string) string {
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return home
	}
	lexical, e1 := os.Stat(filepath.Clean(home))
	physical, e2 := os.Stat(resolved)
	if e1 == nil && e2 == nil && os.SameFile(lexical, physical) {
		return home
	}
	return resolved
}

// usableFile is a regular file, or a link that reaches one. A directory or a dangling link with a
// matching name is not a database, and must not hide the one that is.
func usableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Missing home is null; sorted names break Number ties.
func latestVersionedDb(home, prefix string) (string, error) {
	if _, err := os.Stat(home); err != nil {
		return "", nil
	}
	home = physicalHome(home)
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
		if (best == "" || number > bestNumber) && usableFile(filepath.Join(home, name)) {
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
