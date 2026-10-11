// Package crwconfig resolves crw's named roots: the directories the product keeps its
// tools, cache, scratch space, worktrees, evidence, temporary files, bulk data and
// management state in. Every root has an XDG default, so a clean home needs no
// configuration at all, and a host that keeps its data elsewhere overrides the roots
// it cares about through the override input Resolve takes.
package crwconfig

import (
	"fmt"
	"path/filepath"
	"strings"
)

// The named roots. Each is a directory the product uses, and a configuration file
// overrides one of them by name.
const (
	RootTools    = "tools_root"
	RootCache    = "cache_root"
	RootScratch  = "scratch_root"
	RootWorktree = "worktree_root"
	RootEvidence = "evidence_root"
	RootTemp     = "temp_root"
	RootData     = "data_root"
	RootManage   = "manage_state"
	// RootSandbox is the base path a sandboxed Codex runs in (CRW-1265, internal/sandboxroot). It is a
	// directory the product names, not one it derives from the cache root: moving scratch_root would
	// move every harness that uses it.
	RootSandbox = "sandbox_root"

	// RootBuildCache is Go's build cache (GOCACHE), RootModuleCache its module cache
	// (GOMODCACHE) and RootBuildWork its build work directory (GOTMPDIR). They have no
	// default of their own: unset, the tool keeps its own default and the product passes it
	// nothing, so a clean HOME on one disk needs no tuning.
	RootBuildCache  = "build_cache"
	RootModuleCache = "module_cache"
	RootBuildWork   = "build_work"
	// RootLog is where the product's own long-lived logs go once a host moves them.
	RootLog = "log_root"
	// RootRelayState is the durable relay state root: the directory whose children are the
	// relay's per-scope store directories, each one bundle of the store, its WAL and its
	// sidecars. It is separate from manage_state, which is the management tool's own state.
	RootRelayState = "relay_state"
)

// DefaultSandboxRoot is sandbox_root's default. It is outside the account home, decision C names
// the host's scratch disk, and the coordinator's sandboxed runs already work below it. It is the one
// host path product code names (TestNoHostPathOrDeviceNameInProduct allows exactly this line).
const DefaultSandboxRoot = "/scratch/crw"

// rootOrder is every root, in the order the decided layout lists them: the order
// Resolve checks them in and the order a caller that prints them walks.
var rootOrder = []string{RootTools, RootCache, RootScratch, RootWorktree, RootEvidence, RootTemp, RootData, RootManage,
	RootBuildCache, RootModuleCache, RootBuildWork, RootLog, RootRelayState, RootSandbox}

// RootNames is the root names in the decided order, so a caller that prints the roots
// has one fixed order to follow. The slice is a copy, so a caller cannot change the
// order the package resolves in.
func RootNames() []string { return append([]string(nil), rootOrder...) }

// rootJoin joins rel below base as raw text: the base's trailing separators are
// dropped and one "/" is written between the base and rel. Nothing is cleaned, so a
// base whose spelling mixes a symbolic link and ".." keeps the meaning the
// filesystem gives that spelling instead of the different directory filepath.Clean
// would name. rel is package text ("crw", "tools", ".local/share"), never caller
// input. An empty base yields the relative rel, so an empty HOME still resolves to a
// relative path the absolute-path check refuses rather than to the filesystem root.
func rootJoin(base string, rel ...string) string {
	suffix := strings.Join(rel, "/")
	if base == "" {
		return suffix
	}
	return strings.TrimRight(base, "/") + "/" + suffix
}

// JoinRoot joins rel below base as raw text, the way this package joins its own roots: the base's
// trailing separators are dropped and one "/" is written between the base and rel, and nothing is
// cleaned. A caller that builds a path under a configured root uses this so the root keeps the
// meaning the filesystem gives its spelling: a base that mixes a symbolic link and ".." names the
// directory the kernel resolves it to, not the different directory filepath.Clean would name.
func JoinRoot(base string, rel ...string) string { return rootJoin(base, rel...) }

// Source names where a value came from: the built-in default, an environment
// variable, or the override input.
type Source string

const (
	SourceDefault Source = "default"
	SourceEnv     Source = "env"
	SourceConfig  Source = "config"
)

// Root is one resolved root: the absolute path and where that path came from. A root that
// has no default and that nothing configured (build_cache, module_cache, build_work) has an
// empty Path: it is unset, and the product passes the tool nothing for it.
type Root struct {
	Path   string
	Source Source
}

// Set reports whether the root names a path.
func (r Root) Set() bool { return r.Path != "" }

// sourceOf names where a default came from: an environment variable when one supplied
// the base directory, the built-in default otherwise.
func sourceOf(fromEnv bool) Source {
	if fromEnv {
		return SourceEnv
	}
	return SourceDefault
}

// baseDir is an XDG base directory: the variable's value when it is set and not
// empty, else name below HOME. An empty variable is unset, so a host that exports it
// as "" keeps the fallback rather than resolving against the working directory.
func baseDir(getenv func(string) string, variable, name string) (string, bool) {
	if value := getenv(variable); value != "" {
		return value, true
	}
	return rootJoin(getenv("HOME"), name), false
}

// unsetRoots are the roots with no default: a tool's own default stands until a
// configuration names a path.
var unsetRoots = []string{RootBuildCache, RootModuleCache, RootBuildWork}

func isUnsetRoot(name string) bool {
	for _, unset := range unsetRoots {
		if unset == name {
			return true
		}
	}
	return false
}

// Resolve computes every root from the environment, then applies the overrides. A root
// the overrides name wins and is sourced to them; a root they do not name keeps the
// environment's value, sourced to the variable that supplied it or to the built-in
// default. Every resolved path must be absolute, because a relative one would be read
// wherever the command happens to run. A name this build does not know is left out
// rather than refused, so a configuration written for a later crw still resolves.
//
// A path is joined, never cleaned: a base directory keeps its raw text, so a value
// that spells a symbolic link or ".." means the directory the filesystem resolves it
// to, and a root derived from another (scratch_root from cache_root) sits under the
// text its base was given.
func Resolve(getenv func(string) string, overrides map[string]string) (map[string]Root, error) {
	return resolve(getenv, overrides, true)
}

// resolve is Resolve. With strict false a root the overrides do not name is not required to be
// absolute (see load).
func resolve(getenv func(string) string, overrides map[string]string, strict bool) (map[string]Root, error) {
	data, dataEnv := baseDir(getenv, "XDG_DATA_HOME", ".local/share")
	cache, cacheEnv := baseDir(getenv, "XDG_CACHE_HOME", ".cache")
	state, stateEnv := baseDir(getenv, "XDG_STATE_HOME", ".local/state")
	temp, tempEnv := "/tmp", false
	if value := getenv("TMPDIR"); value != "" {
		temp, tempEnv = value, true
	}
	roots := map[string]Root{}
	for _, row := range []struct {
		name    string
		path    string
		fromEnv bool
	}{
		{RootTools, rootJoin(data, "crw", "tools"), dataEnv},
		{RootCache, rootJoin(cache, "crw"), cacheEnv},
		{RootWorktree, rootJoin(data, "crw", "worktrees"), dataEnv},
		{RootEvidence, rootJoin(state, "crw", "evidence"), stateEnv},
		{RootTemp, rootJoin(temp, "crw"), tempEnv},
		{RootData, rootJoin(data, "crw", "data"), dataEnv},
		{RootManage, rootJoin(state, "crw", "manage"), stateEnv},
		{RootSandbox, DefaultSandboxRoot, false},
		{RootLog, rootJoin(state, "crw", "logs"), stateEnv},
		{RootRelayState, rootJoin(state, "codex-session-relay"), stateEnv},
	} {
		roots[row.name] = Root{Path: row.path, Source: sourceOf(row.fromEnv)}
	}
	// scratch_root follows the resolved cache_root, so it is derived before the
	// overrides are applied and again after them: an override of only cache_root moves
	// the scratch root under the new cache root, and an override that names
	// scratch_root itself keeps the value it named.
	for _, name := range unsetRoots {
		roots[name] = Root{Source: SourceDefault}
	}
	cacheRoot := roots[RootCache]
	roots[RootScratch] = Root{Path: rootJoin(cacheRoot.Path, "scratch"), Source: cacheRoot.Source}
	for name, value := range overrides {
		if _, known := roots[name]; !known {
			continue
		}
		roots[name] = Root{Path: value, Source: SourceConfig}
	}
	if _, named := overrides[RootScratch]; !named {
		cacheRoot = roots[RootCache]
		roots[RootScratch] = Root{Path: rootJoin(cacheRoot.Path, "scratch"), Source: cacheRoot.Source}
	}
	for _, name := range rootOrder {
		root := roots[name]
		if !root.Set() && root.Source == SourceDefault && isUnsetRoot(name) {
			continue
		}
		if !strict && root.Source != SourceConfig {
			continue
		}
		if path := root.Path; !filepath.IsAbs(path) {
			return nil, fmt.Errorf("%s %q: not an absolute path", name, path)
		}
	}
	return roots, nil
}
