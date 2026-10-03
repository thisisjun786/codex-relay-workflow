package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

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
}

func runRepoMap(c invocation) int {
	d := mapDeps{
		run: func(command string, args []string, quiet bool) (int, error) {
			cmd := exec.CommandContext(c.ctx, command, args...)
			if !quiet {
				cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, c.stdout, c.stderr
			}
			err := cmd.Run()
			if cmd.ProcessState == nil {
				return -1, err
			}
			return cmd.ProcessState.ExitCode(), err
		},
		exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		remove: os.RemoveAll,
	}
	return launchRepoMap(c.args, os.LookupEnv, c.stderr, d)
}

func launchRepoMap(args []string, env host.LookupEnv, stderr io.Writer, d mapDeps) int {
	p, err := repoMapPaths(env)
	if err != nil {
		fmt.Fprintln(stderr, "crw map:", err)
		return 1
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
