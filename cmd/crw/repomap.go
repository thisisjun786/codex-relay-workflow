package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

type mapPaths struct {
	script, requirements, python string
	hasUv, hasVenv               bool
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
	return mapPaths{script: filepath.Join(dir, "repomap.py"), requirements: filepath.Join(dir, "requirements.txt"), python: py}, err
}

type mapDeps struct {
	run    func(string, []string, bool) (int, error)
	exists func(string) bool
	remove func(string) error
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
// (<codex>/plugins/cache/<marketplace>/crw/<version>/skills). It answers only when the default
// script, the one a skills link gives, is absent. A cached copy never displaces the active
// PLUGIN_ROOT, however recently it was written (CRW-392).
func installedRepoMapDir(env host.LookupEnv, defaultScript string) string {
	if regularFile(defaultScript) {
		return ""
	}
	skill := filepath.Join("skills", "crw-repo-map", "scripts")
	if root, _ := env("PLUGIN_ROOT"); text.Trim(root) != "" {
		if dir := filepath.Join(root, skill); regularFile(filepath.Join(dir, "repomap.py")) {
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
	cached, _ := filepath.Glob(filepath.Join(codex, "plugins", "cache", "*", "crw", "*", skill))
	best, bestTime := "", time.Time{}
	for _, dir := range cached {
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
	p.hasVenv = d.exists(p.python)
	if bootstrap, _ := env("CRW_MAP_BOOTSTRAP"); !p.hasVenv && bootstrap == "1" && override == "" {
		ok, exit := bootstrapRepoMapVenv(p, stderr, d)
		if exit != 0 {
			return exit
		}
		p.hasVenv = ok
	}
	// uv is probed only for a run it can serve: an explicit interpreter and help (above) never reach it.
	if override == "" {
		code, err := d.run("uv", []string{"--version"}, true)
		p.hasUv = err == nil && code == 0
	}
	return runRepoMapCommand(selectRepoMapCommand(args, env, p), stderr, d)
}

// bootstrapRepoMapVenv builds the one-time venv under the bootstrap lock, so concurrent runs wait for one another and the
// second finds the first's venv. A failed attempt removes only a directory this attempt made: a tree that was there before it
// (another run's, or an earlier partial one) is never deleted (CRW-1147). It returns whether the venv is usable, or a nonzero
// exit when the cleanup itself failed.
func bootstrapRepoMapVenv(p mapPaths, stderr io.Writer, d mapDeps) (ok bool, exit int) {
	dir := filepath.Dir(filepath.Dir(p.python))
	if d.lock != nil {
		unlock, err := d.lock(filepath.Dir(dir))
		if err != nil {
			fmt.Fprintln(stderr, "crw map: venv bootstrap skipped:", err)
			return false, 0
		}
		defer unlock()
		if d.exists(p.python) {
			return true, 0 // the run that held the lock before this one built it
		}
	}
	existed := d.exists(dir)
	fmt.Fprintf(stderr, "crw map: bootstrapping venv at %s (one-time)...\n", dir)
	if code, _ := d.run("python3", []string{"-m", "venv", dir}, false); code != 0 {
		return false, 0
	}
	if code, _ := d.run(p.python, []string{"-m", "pip", "install", "-q", "-r", p.requirements}, false); code == 0 {
		return true, 0
	}
	fmt.Fprintln(stderr, "crw map: venv bootstrap failed; falling back.")
	if !existed {
		if err := d.remove(dir); err != nil {
			fmt.Fprintln(stderr, "crw map:", err)
			return false, 1
		}
	}
	return false, 0
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
