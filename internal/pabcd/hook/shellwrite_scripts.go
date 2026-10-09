package hook

import (
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// Carried shell files reuse the bounded regular-file reader and stale-file
// checks of the GitHub guard. No file changed by this command can be trusted.
func shellIRScriptDests(e shellir.Exec, cwd string, resolve bool, depth int, outer *githubPostWrites) []string {
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
	res, err := shellir.AnalyzeScript(body, e.Dir.Path, e.Cdpath)
	if err != nil {
		return unknown
	}
	for _, exec := range res.Execs {
		if exec.Inline != nil && exec.Inline.Language == "python" {
			if _, bad := shellIRProgramUnreadable(exec.Inline.Source.Value); bad {
				return unknown
			}
		}
	}
	return shellIRDestsResult(res, cwd, resolve, depth+1, outer)
}

// This deliberately conservative lexical rule only expands read-only awk.
// Any redirect/pipe or system/getline operation keeps the unknown destination.
func shellIRAwkReadOnly(src string) bool {
	return !strings.ContainsAny(src, ">|") && !regexp.MustCompile(`\bsystem\s*\(`).MatchString(src) && !(strings.Contains(src, "<") && regexp.MustCompile(`\bgetline\b`).MatchString(src))
}
