//go:build dev

package cxcfuzz

import (
	"math/rand"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// OracleRootEnv names the extracted CXC v0.2.40 component tree directly. It is the explicit input of
// OracleRoot and wins over the configured location.
const OracleRootEnv = "CRW_CXC_ORACLE_ROOT"

// oracleTree is where the extracted CXC v0.2.40 tree sits below the tools root, and oracleComponents
// the component directory below that tree.
const (
	oracleTree       = "cxc-v0.2.40"
	oracleComponents = "plugins/codexclaw/components"
)

// OracleRoot is the extracted CXC v0.2.40 component tree a shim imports its dist modules under, the
// way the record-oracle.mjs recorders do (CRW-958). The tree has no place of its own on a host: it is
// the directory CRW_CXC_ORACLE_ROOT names, else cxc-v0.2.40/plugins/codexclaw/components below the
// configured tools_root (crw config, paths.tools_root; its XDG default on a clean home). A target's
// Oracle.Root overrides it. It is "" only when no root can be named at all (no home), and a run then
// finds no oracle, as it does for a tree that is not there.
func OracleRoot() string { return OracleRootFrom(os.Getenv) }

// OracleRootFrom is OracleRoot reading the environment through getenv.
func OracleRootFrom(getenv func(string) string) string {
	if root := getenv(OracleRootEnv); root != "" {
		return root
	}
	file, err := crwconfig.Load(getenv, "")
	if err != nil {
		return ""
	}
	return crwconfig.JoinRoot(file.Roots()[crwconfig.RootTools].Path, oracleTree, oracleComponents)
}

// echoTarget is the harness's own subject: the Go side answers its input unchanged and the shim
// echoes it, so the two sides agree everywhere until a shim-side mutation is switched on. The
// eight real M7.5 targets replace it as the subjects; it stays as the harness's self-test.
func echoTarget() Target {
	return Target{
		Name:     "echo",
		Generate: echoGenerate,
		Go:       echoGo,
		Oracle:   Oracle{Command: "node", Shim: shimPath("echo"), Root: OracleRoot()},
		Compare:  compareJSON,
	}
}

// shimPath is a target's worker program resolved against this checkout.
func shimPath(target string) string {
	root, err := repositoryRoot()
	if err != nil {
		return filepath.Join("testdata", target, "shim.mjs")
	}
	return filepath.Join(root, "internal", "dev", "cxcfuzz", "testdata", target, "shim.mjs")
}

// echoGo is the echo target's Go side: the identity.
func echoGo(input any, env Env) (any, error) { return input, nil }

// echoGenerate builds a small JSON value: a string, an integer, a boolean, a nested array, and the
// "mutate" marker the shim reads. It never emits a float, because Go spells float64(100) as 100.0
// where the oracle's 100 stays 100, a difference the harness would have invented itself.
func echoGenerate(rng *rand.Rand, size int) any {
	words := []string{"", "a", "hello", "지난번", "\xed\xa0\x80"}
	value := pyjson.Object{
		{Key: "text", Value: words[rng.Intn(len(words))]},
		{Key: "n", Value: rng.Intn(64)},
		{Key: "flag", Value: rng.Intn(2) == 0},
	}
	if rng.Intn(4) == 0 {
		value = value.Set("list", []any{rng.Intn(8), "x"})
	}
	if rng.Intn(8) == 0 {
		value = value.Set("mutate", true)
	}
	return value
}
