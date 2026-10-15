package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// repositoryTools are the repository tools every agent call of a run is given (CRW-1488, 1209 trial finding 12): the go the
// verifier runs, from verifier.go_root of the run's configuration file and nothing else. Env are the entries the verifier's own
// command gets (crwconfig.VerifierTools.GoEnv over the run process's PATH): <go_root>/bin first on PATH, GOROOT=<go_root>,
// GOTOOLCHAIN=local. Prefix is the same entries with the inherited PATH kept as $PATH, and the fact names the go, the setting and
// the file. A configuration that names no go_root, a verifier section the verifier ignores, and a file that cannot be read give
// nil: the calls get nothing, as before (the lane's own reading of the verifier section reports an unreadable file).
func repositoryTools(getenv func(string) string) *engine.RepositoryTools {
	tools, err := crwconfig.LoadVerifierTools(getenv, "")
	if err != nil || tools.Ignored != "" || tools.GoRoot == "" {
		return nil
	}
	bin := filepath.Join(tools.GoRoot, "bin")
	return &engine.RepositoryTools{
		Env:    tools.GoEnv(getenv("PATH")),
		Prefix: "PATH=" + shellWord(bin) + `:"$PATH" GOROOT=` + shellWord(tools.GoRoot) + " GOTOOLCHAIN=local",
		Facts: []string{fmt.Sprintf("go: %s (verifier.go_root %s in %s), the go crw.verify runs, with GOROOT %s and GOTOOLCHAIN=local",
			filepath.Join(bin, "go"), tools.GoRoot, tools.Config, tools.GoRoot)},
	}
}

// shellWord is s as one POSIX shell word: as it is when it holds only characters a shell takes literally, else single-quoted.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
