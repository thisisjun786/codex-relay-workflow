package hook

import (
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
	"strings"
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
	return &memoryShellAnalysis{cached: true, command: command, dir: dir, lookup: lookup, env: shellir.AnalyzeEnvProvenDirectory, plain: shellir.Analyze, noDir: shellir.AnalyzeNoDir}
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

// unreadable says whether any of the three readings of the command fails, and what the reader could not read: the cause of the
// first refusal, or the reason its f-string check names. The order is the gate's: the reading with the session's environment,
// then the readings with none and in no directory.
func (a *memoryShellAnalysis) unreadable() (string, bool) {
	for _, run := range []func() (shellir.Result, error){a.withEnv, a.withoutEnv, a.withoutDir} {
		res, err := run()
		if err != nil {
			return unreadableCause(err), true
		}
		if what, bad := shellIRFStringResult(res, nil); bad {
			return unreadableLabel(what), true
		}
	}
	return "", false
}

// unreadableCause is the reader's own reason for a refusal, bounded and free of control characters. Reasons are fixed texts of
// the reader that name a position, an option or a file below the working directory, never a value of the command.
func unreadableCause(err error) string {
	var u *shellir.Unreadable
	if errors.As(err, &u) {
		return unreadableLabel(u.Reason)
	}
	return ""
}

func unreadableLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return memoryGateLabelLimit(strings.TrimSpace(s), 200)
}

// commandUnreadableReason is the refusal of a command the reader cannot read, shared by the memory and GitHub guards so that the
// two denials of one command say the same thing: what could not be read and what to do, never that a protected write or a GitHub
// post was observed. names is the sentence a guard adds about its own subject (the GitHub guard says the text names no post).
func commandUnreadableReason(rule, reason string, leaf bool, names string) string {
	out := "[crw command-reader] Cannot analyze this command (" + rule + ")"
	if reason != "" {
		out += ": " + reason
	}
	out += ". "
	if names != "" {
		out += names + " "
	}
	if leaf {
		return out + "Report the blocked command and this reason to your parent."
	}
	return out + "Run a readable script file, or change the part the reason names."
}
