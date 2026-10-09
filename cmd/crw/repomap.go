package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// selectRepoMapCommand is the POSIX ladder in bin/codexclaw.mjs:335-358.
func selectRepoMapCommand(args []string, env host.LookupEnv, p mapPaths) mapCommand {
	override, _ := env("CRW_PYTHON")
	help := slices.Contains(args, "--help") || slices.Contains(args, "-h")
	pythonArgs := append([]string{"-B", p.script}, args...)
	switch {
	case !help && text.Trim(override) != "":
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
	// installedSkill names the scripts directory of the plugin-installed crw-repo-map skill when the
	// default skills-link script is absent (CRW-392); nil or an empty answer keeps the default.
	installedSkill func(env host.LookupEnv, defaultScript string) string
}

// installedRepoMapDir is the scripts directory of the repo-map skill a plugin installation keeps
// under the Codex home's plugin cache (<codex>/plugins/cache/<marketplace>/crw/<version>/skills),
// or under PLUGIN_ROOT when the host provides it. It answers only when the default script, the one
// a skills link gives, is absent; of several installed versions the most recently written wins.
func installedRepoMapDir(env host.LookupEnv, defaultScript string) string {
	if info, err := os.Stat(defaultScript); err == nil && info.Mode().IsRegular() {
		return ""
	}
	skill := filepath.Join("skills", "crw-repo-map", "scripts")
	var candidates []string
	if root, _ := env("PLUGIN_ROOT"); text.Trim(root) != "" {
		candidates = append(candidates, filepath.Join(root, skill))
	}
	codex, _ := env("CODEX_HOME")
	if codex == "" {
		if home, err := host.Home(env); err == nil {
			codex = filepath.Join(home, ".codex")
		}
	}
	if codex != "" {
		cached, _ := filepath.Glob(filepath.Join(codex, "plugins", "cache", "*", "crw", "*", skill))
		candidates = append(candidates, cached...)
	}
	best, bestTime := "", time.Time{}
	for _, dir := range candidates {
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

func runRepoMap(c invocation) int {
	d := mapDeps{
		run: func(command string, args []string, quiet bool) (int, error) {
			cmd := exec.CommandContext(c.ctx, command, args...)
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
	p.hasVenv = d.exists(p.python)
	bootstrap, _ := env("CRW_MAP_BOOTSTRAP")
	override, _ := env("CRW_PYTHON")
	if !p.hasVenv && bootstrap == "1" && override == "" {
		dir := filepath.Dir(filepath.Dir(p.python))
		fmt.Fprintf(stderr, "crw map: bootstrapping venv at %s (one-time)...\n", dir)
		if code, _ := d.run("python3", []string{"-m", "venv", dir}, false); code == 0 {
			code, _ := d.run(p.python, []string{"-m", "pip", "install", "-q", "-r", p.requirements}, false)
			p.hasVenv = code == 0
			if !p.hasVenv {
				fmt.Fprintln(stderr, "crw map: venv bootstrap failed; falling back.")
				if err := d.remove(dir); err != nil {
					fmt.Fprintln(stderr, "crw map:", err)
					return 1
				}
			}
		}
	}
	code, err := d.run("uv", []string{"--version"}, true)
	p.hasUv = err == nil && code == 0
	sel := selectRepoMapCommand(args, env, p)
	code, err = d.run(sel.cmd, sel.args, false)
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
