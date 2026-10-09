package hook

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// Analysis is invocation-local. Consumers only read the records and errors;
// distinct directory/environment contexts must retain distinct judgements.
type memoryShellAnalysis struct {
	command, dir                        string
	lookup                              host.LookupEnv
	env                                 func(string, string, func(string) (string, bool)) (shellir.Result, error)
	plain                               func(string, string) (shellir.Result, error)
	noDir                               func(string) (shellir.Result, error)
	cached                              bool
	envResult, plainResult, noDirResult memoryAnalysisResult
}

func newMemoryShellAnalysis(command, dir string, lookup host.LookupEnv) *memoryShellAnalysis {
	return &memoryShellAnalysis{cached: true, command: command, dir: dir, lookup: lookup, env: shellir.AnalyzeEnv, plain: shellir.Analyze, noDir: shellir.AnalyzeNoDir}
}

type memoryAnalysisResult struct {
	result shellir.Result
	err    error
	done   bool
}

// Results, including errors, are reused only within this one gate call.
func (a *memoryShellAnalysis) get(slot *memoryAnalysisResult, run func() (shellir.Result, error)) (shellir.Result, error) {
	if !a.cached {
		return run()
	}
	if !slot.done {
		slot.result, slot.err = run()
		slot.done = true
	}
	return slot.result, slot.err
}
func (a *memoryShellAnalysis) withEnv() (shellir.Result, error) {
	return a.get(&a.envResult, func() (shellir.Result, error) { return a.env(a.command, a.dir, a.lookup) })
}
func (a *memoryShellAnalysis) withoutEnv() (shellir.Result, error) {
	return a.get(&a.plainResult, func() (shellir.Result, error) { return a.plain(a.command, a.dir) })
}
func (a *memoryShellAnalysis) withoutDir() (shellir.Result, error) {
	return a.get(&a.noDirResult, func() (shellir.Result, error) { return a.noDir(a.command) })
}
func (a *memoryShellAnalysis) readable() bool {
	if _, err := a.withEnv(); err != nil {
		return false
	}
	if _, err := a.withoutEnv(); err != nil {
		return false
	}
	_, err := a.withoutDir()
	return err == nil
}
