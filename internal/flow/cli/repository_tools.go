package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// repositoryTools are the repository tools every agent call of a run is given (CRW-1488, 1209 trial finding 12; CRW-1570): the run's
// recorded TMPDIR and the go the verifier runs.
//
// rec is the environment the run's spec records (Spec.Env, written once by `crw flow start`): its TMPDIR, when there is one, comes
// first, in Env as TMPDIR=<dir> (an App Server thread's shell gets it as a leaf key), in Prefix as TMPDIR=<dir as one shell word> (a
// session subagent's shell is the owner session's, which the run cannot set), and in a fact that names the directory and run.json.
// It is the recorded value, never the caller's TMPDIR: a run relaunched from a shell with another one, or none, keeps its own.
//
// The go is from verifier.go_root of the run's configuration file and nothing else. Its Env are the entries the verifier's own
// command gets (crwconfig.VerifierTools.GoEnv over the run process's PATH): <go_root>/bin first on PATH, GOROOT=<go_root>,
// GOTOOLCHAIN=local. Its prefix is the same entries with the inherited PATH kept as $PATH, and its fact names the go, the setting and
// the file. A configuration that names no go_root, a verifier section the verifier ignores, and a file that cannot be read give no go
// (the lane's own reading of the verifier section reports an unreadable file): the TMPDIR alone when one is recorded, else nil, and
// the calls get nothing, as before.
func repositoryTools(getenv func(string) string, rec map[string]string) *engine.RepositoryTools {
	var env, prefix, facts []string
	if dir := rec["TMPDIR"]; dir != "" {
		env = append(env, "TMPDIR="+dir)
		prefix = append(prefix, "TMPDIR="+shellWord(dir))
		facts = append(facts, fmt.Sprintf("temporary directory: %s, the run's recorded TMPDIR (env.TMPDIR in the run's run.json, "+
			"<state>/flow/<run>/run.json), which every command of the call takes", dir))
	}
	if tools, err := crwconfig.LoadVerifierTools(getenv, ""); err == nil && tools.Ignored == "" && tools.GoRoot != "" {
		bin := filepath.Join(tools.GoRoot, "bin")
		env = append(env, tools.GoEnv(getenv("PATH"))...)
		prefix = append(prefix, "PATH="+shellWord(bin)+`:"$PATH" GOROOT=`+shellWord(tools.GoRoot)+" GOTOOLCHAIN=local")
		facts = append(facts, fmt.Sprintf("go: %s (verifier.go_root %s in %s), the go crw.verify runs, with GOROOT %s and GOTOOLCHAIN=local",
			filepath.Join(bin, "go"), tools.GoRoot, tools.Config, tools.GoRoot))
	}
	if len(env) == 0 {
		return nil
	}
	return &engine.RepositoryTools{Env: env, Prefix: strings.Join(prefix, " "), Facts: facts}
}

// shellWord is s as one POSIX shell word: as it is when it holds only characters a shell takes literally, else single-quoted.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
