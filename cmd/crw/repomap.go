package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

type mapPaths struct {
	script, requirements, python string
	// marker is the venv's completion marker: the bootstrap writes it after pip has installed the requirements, and a venv is
	// ready only while it exists (CRW-1162).
	marker         string
	hasUv, hasVenv bool
}
type mapCommand struct {
	cmd  string
	args []string
}

// repoMapOverride is CRW_PYTHON trimmed once: a value of only whitespace is unset for the ladder, the fallback and the
// bootstrap alike (CRW-1147; the oracle reused it as a literal command and let it suppress the bootstrap).
func repoMapOverride(env host.LookupEnv) string {
	override, _ := env("CRW_PYTHON")
	return text.Trim(override)
}

// repoMapHelp is an explicit help request: it only prints usage, so it needs neither a venv nor uv.
func repoMapHelp(args []string) bool {
	return slices.Contains(args, "--help") || slices.Contains(args, "-h")
}

// selectRepoMapCommand is the POSIX ladder in bin/codexclaw.mjs:335-358.
func selectRepoMapCommand(args []string, env host.LookupEnv, p mapPaths) mapCommand {
	override := repoMapOverride(env)
	help := repoMapHelp(args)
	pythonArgs := append([]string{"-B", p.script}, args...)
	switch {
	case !help && override != "":
		return mapCommand{override, pythonArgs}
	case !help && p.hasUv:
		return mapCommand{"uv", append([]string{"run", "--quiet", "--with-requirements", p.requirements, "python", "-B", p.script}, args...)}
	case !help && p.hasVenv:
		return mapCommand{p.python, pythonArgs}
	case override != "":
		return mapCommand{override, pythonArgs}
	default:
		return mapCommand{"python3", pythonArgs}
	}
}

// repoMapVenvMarker is the file inside the venv directory that says the bootstrap finished (CRW-1162).
const repoMapVenvMarker = ".crw-ready"

func repoMapVenvPython(env host.LookupEnv) (string, error) {
	base, err := host.CRWHome(env)
	return filepath.Join(base, "venvs", "repomap", "bin", "python3"), err
}

// The staged skill root is selectable without activating the plugin. The normal
// skill-link destination is CODEX_HOME/skills, not the runtime binary cache.
func repoMapPaths(env host.LookupEnv) (mapPaths, error) {
	root, _ := env("CRW_SKILLS_DIR")
	if text.Trim(root) == "" {
		codex, _ := env("CODEX_HOME")
		if codex == "" {
			home, err := host.Home(env)
			if err != nil {
				return mapPaths{}, err
			}
			codex = filepath.Join(home, ".codex")
		}
		root = filepath.Join(codex, "skills")
	}
	dir := filepath.Join(root, "crw-repo-map", "scripts")
	py, err := repoMapVenvPython(env)
	return mapPaths{script: filepath.Join(dir, "repomap.py"), requirements: filepath.Join(dir, "requirements.txt"), python: py,
		marker: filepath.Join(filepath.Dir(filepath.Dir(py)), repoMapVenvMarker)}, err
}

type mapDeps struct {
	run    func(string, []string, bool) (int, error)
	exists func(string) bool
	remove func(string) error
	// mark writes the venv's completion marker atomically; it is called only after pip has succeeded (CRW-1162). Nil means
	// the marker is not written (tests with fakes that do not care).
	mark func(path string) error
	// lock serializes the venv bootstrap of one crw home: it returns once this process holds the lock on dir's
	// directory (CRW-1147). Nil means no serialization (tests with fakes).
	lock func(dir string) (unlock func(), err error)
	// installedSkill names the scripts directory of the plugin-installed crw-repo-map skill when the
	// default skills-link script is absent (CRW-392); nil or an empty answer keeps the default.
	installedSkill func(env host.LookupEnv, defaultScript string) string
}

// installedRepoMapDir is the scripts directory of the repo-map skill a plugin installation keeps:
// PLUGIN_ROOT's when the host provides it and its repomap.py is a regular file, otherwise the most
// recently written of the versions under the Codex home's plugin cache
// (<codex>/plugins/cache/<marketplace>/crw/<version>). Each plugin's skills directory is the one its
// manifest names (pluginSkillsDir, CRW-1147). It answers only when the default script, the one a
// skills link gives, is absent. A cached copy never displaces the active PLUGIN_ROOT, however
// recently it was written (CRW-392).
func installedRepoMapDir(env host.LookupEnv, defaultScript string) string {
	if regularFile(defaultScript) {
		return ""
	}
	skill := filepath.Join("crw-repo-map", "scripts")
	if root, _ := env("PLUGIN_ROOT"); text.Trim(root) != "" {
		if dir := filepath.Join(pluginSkillsDir(root), skill); regularFile(filepath.Join(dir, "repomap.py")) {
			return dir
		}
	}
	codex, _ := env("CODEX_HOME")
	if codex == "" {
		if home, err := host.Home(env); err == nil {
			codex = filepath.Join(home, ".codex")
		}
	}
	if codex == "" {
		return ""
	}
	versions, _ := filepath.Glob(filepath.Join(codex, "plugins", "cache", "*", "crw", "*"))
	best, bestTime := "", time.Time{}
	for _, version := range versions {
		dir := filepath.Join(pluginSkillsDir(version), skill)
		info, err := os.Stat(filepath.Join(dir, "repomap.py"))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = dir, info.ModTime()
		}
	}
	return best
}

// pluginManifestMaxBytes bounds the plugin manifest read for its skills directory.
const pluginManifestMaxBytes = 1 << 20

// pluginSkillsDir is the skills directory a plugin's manifest names (the "skills" string of
// <root>/.codex-plugin/plugin.json, relative to the plugin root), or <root>/skills, the directory the
// plugin ships today, when the manifest is absent, unreadable or larger than the bound, or names no
// relative directory inside the plugin (CRW-1147).
func pluginSkillsDir(root string) string {
	fallback := filepath.Join(root, "skills")
	manifest := filepath.Join(root, ".codex-plugin", "plugin.json")
	if info, err := os.Stat(manifest); err != nil || !info.Mode().IsRegular() || info.Size() > pluginManifestMaxBytes {
		return fallback
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		return fallback
	}
	var m struct {
		Skills *string `json:"skills"`
	}
	if json.Unmarshal(data, &m) != nil || m.Skills == nil {
		return fallback
	}
	rel := text.Trim(*m.Skills)
	if rel == "" || filepath.IsAbs(rel) {
		return fallback
	}
	if rel = filepath.Clean(rel); rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fallback
	}
	return filepath.Join(root, rel)
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// repoMapProbeTimeout bounds a quiet probe (the uv availability check): a probe that does not answer is a tool that is not there.
var repoMapProbeTimeout = 5 * time.Second

// repoMapLockPoll is how often a waiting bootstrap tries the lock again.
var repoMapLockPoll = 100 * time.Millisecond

// repoMapBootstrapLock takes the exclusive lock that serializes the venv bootstrap of one crw home, waiting for another
// bootstrap to finish until ctx ends. The lock is held on the venvs directory itself, which a failed bootstrap never removes
// (it removes the repomap directory inside), so the lock leaves no file of its own behind.
func repoMapBootstrapLock(ctx context.Context, venvs string, stderr io.Writer) (func(), error) {
	if err := os.MkdirAll(venvs, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(venvs)
	if err != nil {
		return nil, err
	}
	waited := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "crw map: waiting for another venv bootstrap to finish...")
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(repoMapLockPoll):
		}
	}
}

// markerFile and markerOps are the file operations of the marker write, injectable so a test can observe their order.
type markerFile interface {
	Name() string
	WriteString(s string) (int, error)
	Sync() error
	Close() error
}

type markerOps struct {
	createTemp func(dir, pattern string) (markerFile, error)
	rename     func(oldpath, newpath string) error
	remove     func(path string) error
}

var osMarkerOps = markerOps{
	createTemp: func(dir, pattern string) (markerFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return f, nil
	},
	rename: os.Rename,
	remove: os.Remove,
}

// writeRepoMapMarker writes the completion marker atomically: the content goes to a temporary file in the venv directory, is
// synced, and is renamed onto the marker, so the marker either does not exist or is whole, whatever kills the process (CRW-1162).
func writeRepoMapMarker(path string) error { return writeRepoMapMarkerWith(path, osMarkerOps) }

func writeRepoMapMarkerWith(path string, ops markerOps) error {
	f, err := ops.createTemp(filepath.Dir(path), repoMapVenvMarker+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString("crw map venv: requirements installed\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = ops.rename(tmp, path)
	}
	if err != nil {
		_ = ops.remove(tmp)
	}
	return err
}

// repoMapVenvReady is the readiness verdict: the interpreter exists and the bootstrap left its completion marker. An interpreter
// alone is what a killed bootstrap or a failed venv leaves behind (CRW-1162).
func repoMapVenvReady(p mapPaths, d mapDeps) bool {
	return d.exists(p.python) && d.exists(p.marker)
}

func runRepoMap(c invocation) int {
	d := mapDeps{
		lock: func(dir string) (func(), error) { return repoMapBootstrapLock(c.ctx, dir, c.stderr) },
		run: func(command string, args []string, quiet bool) (int, error) {
			ctx := c.ctx
			if quiet {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, repoMapProbeTimeout)
				defer cancel()
			}
			cmd := exec.CommandContext(ctx, command, args...)
			// POSIX oracle lookup permits relative PATH entries.
			if errors.Is(cmd.Err, exec.ErrDot) {
				cmd.Path, cmd.Err = filepath.Abs(cmd.Path)
			}
			if !quiet {
				cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, c.stdout, c.stderr
			}
			err := cmd.Run()
			if cmd.ProcessState == nil {
				return -1, err
			}
			return cmd.ProcessState.ExitCode(), err
		},
		exists:         func(p string) bool { _, err := os.Stat(p); return err == nil },
		remove:         os.RemoveAll,
		mark:           writeRepoMapMarker,
		installedSkill: installedRepoMapDir,
	}
	return launchRepoMap(c.args, os.LookupEnv, c.stderr, d)
}

func launchRepoMap(args []string, env host.LookupEnv, stderr io.Writer, d mapDeps) int {
	p, err := repoMapPaths(env)
	if err != nil {
		fmt.Fprintln(stderr, "crw map:", err)
		return 1
	}
	if root, _ := env("CRW_SKILLS_DIR"); text.Trim(root) == "" && d.installedSkill != nil {
		if dir := d.installedSkill(env, p.script); dir != "" {
			p.script, p.requirements = filepath.Join(dir, "repomap.py"), filepath.Join(dir, "requirements.txt")
		}
	}
	// An explicit help is read-only: it needs no venv, no bootstrap and no uv, so it answers before any of them (CRW-1147).
	if repoMapHelp(args) {
		return runRepoMapCommand(selectRepoMapCommand(args, env, p), stderr, d)
	}
	override := repoMapOverride(env)
	if bootstrap, _ := env("CRW_MAP_BOOTSTRAP"); bootstrap == "1" && override == "" {
		// The bootstrap run takes the lock before it looks for the interpreter: python3 -m venv creates it before pip has
		// installed anything, so an interpreter that exists is not yet a ready venv while another run holds the lock (CRW-1147).
		ok, exit := bootstrapRepoMapVenv(p, stderr, d)
		if exit != 0 {
			return exit
		}
		p.hasVenv = ok
	} else {
		p.hasVenv = repoMapVenvReady(p, d)
	}
	// uv is probed only for a run it can serve: an explicit interpreter and help (above) never reach it.
	if override == "" {
		code, err := d.run("uv", []string{"--version"}, true)
		p.hasUv = err == nil && code == 0
	}
	return runRepoMapCommand(selectRepoMapCommand(args, env, p), stderr, d)
}

// bootstrapRepoMapVenv builds the one-time venv under the bootstrap lock, so concurrent runs wait for one another and the
// second finds the first's venv. A venv is ready when its completion marker exists, which is written only after pip succeeded: an
// interpreter without the marker (a bootstrap that was killed, or a venv from before the marker) is built again, once (CRW-1162).
// A failed venv or pip removes the directory only when this attempt made it: a tree that was there before (another run's, or an
// earlier partial one) is never deleted, and neither is an interpreter that was there before; only an interpreter this attempt
// put in it goes (CRW-1147, CRW-1162). It returns whether the venv is
// usable, or a nonzero exit when the cleanup itself failed.
func bootstrapRepoMapVenv(p mapPaths, stderr io.Writer, d mapDeps) (ok bool, exit int) {
	dir := filepath.Dir(filepath.Dir(p.python))
	if d.lock != nil {
		unlock, err := d.lock(filepath.Dir(dir))
		if err != nil {
			fmt.Fprintln(stderr, "crw map: venv bootstrap skipped:", err)
			return repoMapVenvReady(p, d), 0 // without the lock nothing is built, and a ready venv is used as before
		}
		defer unlock()
	}
	if repoMapVenvReady(p, d) {
		return true, 0 // ready: any earlier run finished (or failed and cleaned up) before it released the lock
	}
	// A marker without its interpreter is stale; it goes before the venv is built again, or a kill during the build would leave
	// an interpreter that looks ready.
	if d.exists(p.marker) {
		if err := d.remove(p.marker); err != nil {
			fmt.Fprintln(stderr, "crw map:", err)
			return false, 1
		}
	}
	existed, hadPython := d.exists(dir), d.exists(p.python)
	fmt.Fprintf(stderr, "crw map: bootstrapping venv at %s (one-time)...\n", dir)
	if code, _ := d.run("python3", []string{"-m", "venv", dir}, false); code != 0 {
		return false, cleanupRepoMapVenv(p, dir, existed, hadPython, stderr, d)
	}
	if code, _ := d.run(p.python, []string{"-m", "pip", "install", "-q", "-r", p.requirements}, false); code != 0 {
		fmt.Fprintln(stderr, "crw map: venv bootstrap failed; falling back.")
		return false, cleanupRepoMapVenv(p, dir, existed, hadPython, stderr, d)
	}
	if d.mark != nil {
		if err := d.mark(p.marker); err != nil {
			fmt.Fprintln(stderr, "crw map: venv bootstrap failed:", err)
			fmt.Fprintln(stderr, "crw map: falling back.")
			return false, cleanupRepoMapVenv(p, dir, existed, hadPython, stderr, d)
		}
	}
	return true, 0
}

// cleanupRepoMapVenv removes what a failed attempt made. A directory that was there before keeps its tree, and so does an
// interpreter that was there before: an earlier venv (or a legacy one) is not this attempt's to delete, and without the marker it
// is not taken for ready anyway, so keeping it blocks no retry. Only an interpreter this attempt made goes, which leaves the next
// opted-in run a clean place to build in. It returns the exit code of the run: nonzero only when the removal itself failed.
func cleanupRepoMapVenv(p mapPaths, dir string, existed, hadPython bool, stderr io.Writer, d mapDeps) int {
	gone := dir
	if existed {
		if hadPython {
			return 0
		}
		gone = p.python
	}
	if err := d.remove(gone); err != nil {
		fmt.Fprintln(stderr, "crw map:", err)
		return 1
	}
	return 0
}

func runRepoMapCommand(sel mapCommand, stderr io.Writer, d mapDeps) int {
	code, err := d.run(sel.cmd, sel.args, false)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) || code == 127 || code == 9009 {
		exit := "spawn error"
		if code >= 0 {
			exit = fmt.Sprint(code)
		}
		fmt.Fprintf(stderr, "crw map: %s could not be run (exit %s). Install Python 3.9+ or set CRW_PYTHON.\n", sel.cmd, exit)
		return 1
	}
	if code < 0 {
		return 1
	}
	return code
}
