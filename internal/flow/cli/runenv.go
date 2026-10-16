package cli

import (
	"fmt"
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
