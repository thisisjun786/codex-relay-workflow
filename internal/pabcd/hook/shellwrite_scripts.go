package hook

import (
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

func shellIRScriptUsesArgs(body string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(body), "")
	if err != nil {
		return false
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if parameter, ok := node.(*syntax.ParamExp); ok && parameter.Param != nil && parameter.Param.Value == "@" {
			found = true
		}
		return !found
	})
	return found
}

// shellIRDirectForwarder: a shell file run by its path whose body forwards "$@" is read by the memory and worktree guards, so that the
// forward the reader does not follow refuses it (CRW-1283 plan C).
func shellIRDirectForwarder(execution shellir.Exec) bool {
	if !githubPostDirectPath(execution) {
		return false
	}
	body, kind := githubPostReadDirectExec(execution, execution.Dir.Path)
	return kind == githubPostFileShell && shellIRScriptUsesArgs(body)
}

// Carried shell files reuse the bounded regular-file reader and stale-file
// checks of the GitHub guard. No file changed by this command can be trusted.
func shellIRScriptDests(e shellir.Exec, cwd string, lookup func(string) (string, bool), resolve bool, depth int, outer *githubPostWrites) []string {
	unknown := []string{shellIRUnknownDest}
	if depth >= githubPostMaxScriptDepth || !e.Script.Known || !githubPostScriptKnown(e.Script.Value, e.Dir) {
		return unknown
	}
	body, ok := githubPostReadScript(e.Script.Value, e.Dir.Path)
	if !ok {
		return unknown
	}
	if e.Name == "awk" || e.Name == "gawk" || e.Name == "mawk" || e.Name == "nawk" {
		if shellIRAwkReadOnly(body) {
			return nil
		}
		return unknown
	}
	read, dir := func(body, dir string, cdpath bool) (shellir.Result, error) {
		return shellir.AnalyzeScriptBody(body, dir, cdpath, nil, e.SearchPath)
	}, e.Dir.Path
	if resolve {
		// The script inherits what the text that runs it did to exit and cd (CRW-1165), and the environment the gate is given.
		read = func(body, dir string, cdpath bool) (shellir.Result, error) {
			return shellir.AnalyzeScriptProven(body, dir, shellir.ScriptOptions{Cdpath: cdpath, ExitTainted: e.ExitTainted, Startup: lookup, Body: true, Path: e.SearchPath})
		}
		if !e.Dir.Known {
			dir = ""
		}
	}
	res, err := read(body, dir, e.Cdpath)
	if err != nil {
		return unknown
	}
	for _, exec := range res.Execs {
		// These child programs were opaque on the top-level file path. Reading
		// the enclosing shell file does not prove their effects harmless.
		if exec.Kind == shellir.KindCommand && exec.Inline == nil {
			if regexp.MustCompile(`^(?:python[0-9.]*|py|node|ruby|perl)$`).MatchString(exec.Name) {
				if len(exec.Args) != 1 || !exec.Args[0].Known || !slices.Contains([]string{"-V", "-v", "--version", "-h"}, exec.Args[0].Value) {
					return unknown
				}
			}
		}
		if exec.Inline != nil && exec.Inline.Language == "python" {
			if _, bad := shellIRProgramUnreadable(exec.Inline.Source.Value); bad {
				return unknown
			}
		}
	}
	return shellIRDestsResult(res, cwd, lookup, resolve, depth+1, outer)
}

// This deliberately conservative lexical rule only expands read-only awk.
// Any redirect/pipe or system/getline operation keeps the unknown destination.
func shellIRAwkReadOnly(src string) bool {
	return !strings.ContainsAny(src, ">|") && !regexp.MustCompile(`\bsystem\s*\(`).MatchString(src) && !(strings.Contains(src, "<") && regexp.MustCompile(`\bgetline\b`).MatchString(src))
}
