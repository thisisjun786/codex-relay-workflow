//go:build dev

package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// CRW-964: the pinned tool versions and the ones this host actually has. The pins come from the
// files that fix them (go.mod's toolchain, ci.yml's Node, scripts/ci/secrets.sh's Gitleaks, the
// staticcheck module go.mod requires); a tool whose observed version differs from its pin is
// named in the record's pinMismatch and that record is never reused (answer 3).

// localToolNames is the tools the record names, in a stable order.
var localToolNames = []string{"go", "node", "gitleaks", "staticcheck"}

var (
	goModToolchain = regexp.MustCompile(`(?m)^toolchain go([0-9][^\s]*)$`)
	goModRequire   = regexp.MustCompile(`(?m)^\s*(honnef\.co/go/tools)\s+v([0-9][^\s]*)$`)
	secretsVersion = regexp.MustCompile(`(?m)^scan_version=([0-9][^\s]*)$`)
	goVersionLine  = regexp.MustCompile(`(?m)^go version go([0-9][^\s]*)`)
)

// localToolPins reads the pinned versions from the verified tree. A pin the tree does not carry
// is left empty rather than guessed.
func localToolPinsFrom(read func(path string) ([]byte, error)) (map[string]string, error) {
	pins := map[string]string{}
	if data, err := read("go.mod"); err == nil {
		if m := goModToolchain.FindSubmatch(data); m != nil {
			pins["go"] = string(m[1])
		}
		if m := goModRequire.FindSubmatch(data); m != nil {
			pins["staticcheck"] = string(m[2])
		}
	}
	if data, err := read("scripts/ci/secrets.sh"); err == nil {
		if m := secretsVersion.FindSubmatch(data); m != nil {
			pins["gitleaks"] = string(m[1])
		}
	}
	return pins, nil
}

// localToolVersions observes the tools this host has, resolving each through PATH. A tool that is
// absent is reported with an empty version, so a step that needs it fails as missing_tool rather
// than passing.
func localToolVersions(pathEnv string) map[string]string {
	return localToolVersionsIn(pathEnv, nil, "")
}

// localToolVersionsIn is localToolVersions with the verified commit's go.mod for the Go probe, so the
// toolchain it reports is the one a step in that module selects.
func localToolVersionsIn(pathEnv string, goMod []byte, gate string) map[string]string {
	versions := map[string]string{}
	for _, name := range localToolNames {
		if name == "go" {
			versions[name] = localObserveToolIn(name, pathEnv, goMod, gate)
			continue
		}
		versions[name] = localObserveTool(name, pathEnv, gate)
	}
	return versions
}

// localObservedVersions is what the steps actually used. Two tools are fetched by the step rather
// than inherited: secrets.sh downloads and runs the pinned Gitleaks, and the lint leg runs the
// staticcheck the tree requires. So a host copy of either never runs, and the record names the pin.
func localObservedVersions(versions, pins map[string]string) map[string]string {
	for _, name := range []string{"gitleaks", "staticcheck"} {
		if pins[name] != "" {
			versions[name] = pins[name]
		}
	}
	return versions
}

// localObserveTool is one tool's version, or "" when it is not on PATH or does not answer.
func localObserveTool(name, pathEnv, gate string) string {
	return localObserveToolIn(name, pathEnv, nil, gate)
}

// localObserveToolIn is localObserveTool run in a directory that holds goMod, when given, as go.mod.
func localObserveToolIn(name, pathEnv string, goMod []byte, gate string) string {
	var args []string
	switch name {
	case "go":
		args = []string{"version"}
	case "node":
		args = []string{"--version"}
	case "gitleaks":
		args = []string{"version"}
	case "staticcheck":
		args = []string{"-version"}
	default:
		return ""
	}
	path := localLookPath(name, pathEnv)
	if path == "" {
		return ""
	}
	// The probe runs in a directory of its own, without the caller's GOENV, so the version it reads is
	// the one a step with this PATH runs.
	// the probe makes its directory in TMPDIR, which is never the account's real home (CRW-1186)
	if homeguard.Refuse(localTempDir()) != nil {
		return ""
	}
	dir, err := os.MkdirTemp(localTempDir(), "probe")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	if goMod != nil {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), goMod, 0o644); err != nil {
			return ""
		}
	}
	cmd := localProbeCommand(gate, path, args, dir, localProbeEnv(filepath.Join(dir, "home"), pathEnv))
	cmd.Dir = dir
	// The probe has the steps' isolation: its own HOME and XDG directories, and no GOENV or GOTOOLCHAIN
	// from the caller.
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return ""
	}
	// The probe runs with the steps' sealed environment: the same allowlist, GOTOOLCHAIN local and the
	// engine's GOFLAGS, so the version it reads is the one a step reads.
	cmd.Env = localProbeEnv(home, pathEnv)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return localParseToolVersion(name, string(out))
}

// localLookPath is the executable a PATH names, or "" when no directory on it has one.
func localLookPath(name, pathEnv string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// localParseToolVersion reads a tool's version out of its own output.
func localParseToolVersion(name, output string) string {
	text := strings.TrimSpace(output)
	switch name {
	case "go":
		if m := goVersionLine.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	case "node":
		return strings.TrimPrefix(strings.SplitN(text, "\n", 2)[0], "v")
	case "gitleaks", "staticcheck":
		// Both print their version last (gitleaks: "8.30.1"; staticcheck: "version 0.8.1").
		fields := strings.Fields(text)
		if len(fields) > 0 {
			return strings.TrimPrefix(fields[len(fields)-1], "v")
		}
	}
	return ""
}

// localPinMismatch names every tool whose observed version differs from its pin. A tool the tree
// pins but the host does not have is a mismatch too: the run cannot claim the pinned toolchain.
//
// Two tools are fetched by the step rather than inherited from the host, so an absent host copy is
// not a mismatch: secrets.sh downloads the pinned Gitleaks and verifies its checksum itself, and
// the lint leg runs staticcheck through the module the tree requires. A host copy, when there is
// one, is still compared.
func localPinMismatch(pins, observed map[string]string) []string {
	var mismatches []string
	for _, name := range localToolNames {
		pin, pinned := pins[name]
		if !pinned || pin == "" {
			continue
		}
		seen := observed[name]
		if seen == "" && (name == "gitleaks" || name == "staticcheck") {
			continue
		}
		if seen != pin {
			mismatches = append(mismatches, name)
		}
	}
	return localSortedUnique(mismatches)
}

// localProbeEnv is the environment a tool probe runs with: the steps' allowlist, GOTOOLCHAIN local,
// GOWORK off and the engine's GOFLAGS, so the version a probe reads is the one a step reads (parent
// ruling 2, d1). The caller's Go settings never reach it.
func localProbeEnv(home, pathEnv string) []string {
	env := []string{"PATH=" + pathEnv, "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"), "XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"), "TZ=UTC", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-p=4"}
	for _, name := range localInheritedEnv {
		if name == "PATH" {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// localPinsAt reads the pins a commit names, the tools' and every setup-node pin of its ci.yml, through
// read, which takes a path inside the commit.
func localPinsAt(read func(path string) ([]byte, error)) (map[string]string, []workflowJob, error) {
	pins, err := localToolPinsFrom(read)
	if err != nil {
		return nil, nil, err
	}
	data, err := read(".github/workflows/ci.yml")
	if err != nil {
		return nil, nil, err
	}
	jobs, err := parseWorkflow(string(data))
	if err != nil {
		return nil, nil, err
	}
	var nodePins []string
	for _, job := range jobs {
		for _, step := range job.steps {
			if step.nodeVersion != "" {
				nodePins = append(nodePins, step.nodeVersion)
			}
		}
	}
	if nodes := localSortedUnique(nodePins); len(nodes) > 0 {
		pins["node"] = strings.Join(nodes, ",")
	}
	return pins, jobs, nil
}

// localRecomputedReuse refuses a record whose pin mismatch, recomputed from the verified commit and the
// versions this host observes, is not empty. The record's stored mismatch is never read (parent ruling 2, d3).
func localRecomputedReuse(opts localOptions, plan []localJob, current verificationRecord) (bool, string) {
	pins, jobs, err := localPinsAt(func(path string) ([]byte, error) {
		return runGit(opts.Root, "show", current.HeadCommit+":"+path)
	})
	if err != nil {
		return false, "the commit's pins cannot be read: " + err.Error()
	}
	mismatch := localSortedUnique(append(localPinMismatch(pins, current.Tools), localNodeMismatch(plan, jobs, current.Tools["node"])...))
	if len(mismatch) > 0 {
		return false, "the pins the commit names mismatch (" + strings.Join(mismatch, ", ") + ")"
	}
	return true, "the pins the commit names match"
}

// localProbeCommand is the command a version probe runs. The probe runs through the heavy-check gate when
// one is configured, as the checks it informs do, so the versions a record names are read under the same
// gate as the steps (pre-merge finding d1).
func localProbeCommand(gate, path string, args []string, dir string, env []string) *exec.Cmd {
	words := strings.Fields(gate)
	if len(words) == 0 {
		return exec.Command(path, args...)
	}
	argv := append(append([]string{}, words...), "env", "-i")
	argv = append(argv, env...)
	argv = append(argv, "bash", "-c", localChdirScript, "bash", dir, path)
	argv = append(argv, args...)
	return exec.Command(argv[0], argv[1:]...)
}

// localCheckoutDiff names the first tracked file whose bytes in the clean worktree are not the bytes the
// commit stores, or "" when every file matches. git status compares filtered content, so a smudge or clean
// filter can make a changed file look clean; hashing each file with --no-filters compares the bytes the
// commit holds (pre-merge finding d1). Symlinks are not regular files and are not hashed.
func localCheckoutDiff(worktree, commit string) string {
	listing, err := runGit(worktree, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return "the commit's tree cannot be read: " + err.Error()
	}
	want := map[string]string{}
	var paths []string
	for _, entry := range strings.Split(string(listing), "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) == 3 && fields[0] == "120000" {
			// A link could name a file outside the commit, which the record would never see (pre-merge finding d1).
			return path + " is a symlink; the run reads only the regular files of the commit"
		}
		if len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		if strings.Contains(path, "\n") {
			return path + " has a newline in its name, which the check cannot read"
		}
		want[path] = fields[2]
		paths = append(paths, path)
	}
	cmd := exec.Command("git", "-C", worktree, "hash-object", "--no-filters", "--stdin-paths")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "the clean worktree cannot be hashed: " + err.Error()
	}
	got := strings.Fields(string(out))
	if len(got) != len(paths) {
		return "the clean worktree's hashes do not match its files"
	}
	for i, path := range paths {
		if got[i] != want[path] {
			return path + " differs from the commit"
		}
	}
	return ""
}
