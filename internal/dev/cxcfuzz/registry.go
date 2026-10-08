//go:build dev

package cxcfuzz

import (
	"math/rand"
	"sort"
)

// Kind is what one comparison found.
type Kind string

const (
	// Same: both sides answered the same thing.
	Same Kind = "same"
	// Miss: the oracle refuses an input the Go side accepts (a security target).
	Miss Kind = "miss"
	// Extra: the Go side refuses an input the oracle accepts.
	Extra Kind = "extra"
	// Differ: both answered, differently.
	Differ Kind = "differ"
)

// Verdict is one comparison's outcome.
type Verdict struct {
	Kind   Kind   `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

// Env is the environment one case runs under: the case's own root and the homes under it, so a
// target that reads HOME, CODEX_HOME, CRW_HOME or TMPDIR never reaches a real one. A target reads
// them from here; the harness itself sets nothing in its own process.
type Env struct {
	Root      string
	Home      string
	CodexHome string
	CrwHome   string
	TmpDir    string
}

// Oracle names the worker program a target's oracle side runs: the interpreter to resolve on
// PATH, the program it runs, and the tree that program imports its dist modules under (ORACLE_ROOT).
type Oracle struct {
	Command string
	Shim    string
	Root    string
}

// Target is one differential-fuzz subject: how to generate an input, the Go function under test,
// the oracle worker that answers the same input, and how the two answers are compared.
type Target struct {
	Name     string
	Generate func(rng *rand.Rand, size int) any
	Go       func(input any, env Env) (any, error)
	Oracle   Oracle
	Compare  func(goOut, oracleOut any) Verdict
	// Reading is optional. It says whether an input is a shell command the shared command reader cannot read, and whether the Go
	// answer for it is a refusal. The campaign counts both and fails the run when an unreadable case was not refused (criterion
	// c2g). Only the three command-gate targets set it.
	Reading func(input any, env Env, goOut any) (unreadable, refused bool)
}

// registry is the target table: one entry line per target. It is built on each call rather than
// held in a package-level variable, because this package does no work at program start. The echo
// entry is the harness's own subject; the eight real M7.5 targets add their lines here.
func registry() []Target {
	return []Target{
		echoTarget(),
		shellwriteTarget(),
		memorygateTarget(),
		doctorTarget(),
		worktreeDelTarget(),
		spawnTarget(),
	}
}

// Lookup is the registered target called name.
func Lookup(name string) (Target, bool) {
	for _, target := range registry() {
		if target.Name == name {
			return target, true
		}
	}
	return Target{}, false
}

// Names is the registered target names, sorted.
func Names() []string {
	targets := registry()
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	sort.Strings(names)
	return names
}
