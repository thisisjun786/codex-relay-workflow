package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// runTemp is the temporary location a run records at its start (CRW-1570): the one temp function's answer for the starting
// environment (tmploc.DirFrom), as written, once that directory is known to be usable (tmploc.CheckDir). It is the only place in
// flow that asks tmploc at a start; every later process of the run reads the spec. An error is tmploc's reason, and the start
// refuses on it.
func runTemp(getenv func(string) string) (string, error) {
	dir, _, err := tmploc.DirFrom(getenv)
	if err != nil {
		return "", err
	}
	if _, err := tmploc.CheckDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// checkRunEnv says whether the environment a run's spec records can still be used: nil for a spec without env (a run started
// before the record), otherwise the recorded TMPDIR checked as tmploc checks a directory. The spec is never rewritten, so a
// removed directory is made again.
func checkRunEnv(spec Spec) error {
	dir, ok := spec.Env["TMPDIR"]
	if !ok {
		return nil
	}
	if _, err := tmploc.CheckDir(dir); err != nil {
		return fmt.Errorf("the run's recorded TMPDIR %s is not usable: %v; make the directory again", dir, err)
	}
	return nil
}

// runEnviron is the environment a relaunched run process starts with (CRW-1570): base, the caller's, with each variable the run's spec
// records in place of the caller's value, exactly one entry per recorded variable, the recorded ones last in sorted order. A shell that
// relaunches the run with another TMPDIR, or none, gives the process the run's own.
func runEnviron(base []string, rec map[string]string) []string {
	out := make([]string, 0, len(base)+len(rec))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if _, recorded := rec[name]; !recorded {
			out = append(out, kv)
		}
	}
	names := make([]string, 0, len(rec))
	for name := range rec {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, name+"="+rec[name])
	}
	return out
}

// applyRunEnv is the binary's Deps.ApplyEnv (CRW-1570): it sets each variable a run's spec records in the environment of this process,
// in sorted order, and its restore sets back the value each one had, or unsets one that was not set. An empty record changes nothing. A
// variable that cannot be set puts back the ones set before it and is the error.
func applyRunEnv(rec map[string]string) (restore func(), err error) {
	type was struct {
		name, value string
		set         bool
	}
	var prev []was
	restore = func() {
		for i := len(prev) - 1; i >= 0; i-- {
			if prev[i].set {
				_ = os.Setenv(prev[i].name, prev[i].value)
			} else {
				_ = os.Unsetenv(prev[i].name)
			}
		}
	}
	names := make([]string, 0, len(rec))
	for name := range rec {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value, set := os.LookupEnv(name)
		if err := os.Setenv(name, rec[name]); err != nil {
			restore()
			return nil, fmt.Errorf("the run's recorded %s cannot be set: %w", name, err)
		}
		prev = append(prev, was{name: name, value: value, set: set})
	}
	return restore, nil
}

// applyEnv is the run's recorded env put into this process with the command's ApplyEnv: nothing for a spec without env or a Deps without
// ApplyEnv. The run that works in this process (execute, the foreground `crw flow resume RUN SCRIPT`) calls it after checkRunEnv and
// before it takes a lock, and restores when it is done.
func (c *command) applyEnv(spec Spec) (restore func(), err error) {
	if len(spec.Env) == 0 || c.ApplyEnv == nil {
		return func() {}, nil
	}
	return c.ApplyEnv(spec.Env)
}
