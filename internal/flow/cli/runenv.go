package cli

import (
	"fmt"

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
