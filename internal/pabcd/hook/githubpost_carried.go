package hook

// The secret scan of every post of a refused text (CRW-1307). A refusal of this guard other than a secret now only warns, so
// the text still runs: a secret that a post of that text carries must still be found, whichever execution the refusal came
// from. githubPostCarriedSecret reads every gh execution of the text for the text it posts, without the form rules: an inline
// value, a body or field file, a body on standard input that the command shows, and a body file that the same text writes
// before the post from text it shows. What it cannot read it leaves to the refusal that warns (fail-open, as the table says).

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// githubPostCarriedSecret is the secret-in-github-text site of the first gh execution of execs that carries a secret, with that
// execution's line.
func githubPostCarriedSecret(execs []shellir.Exec) (githubPostSite, int, bool) {
	for i, e := range execs {
		if e.Kind != shellir.KindCommand || githubPostProgram(e.Name) != "gh" {
			continue
		}
		words := githubPostWordsOf(e)
		if place, found := githubPostInlineSecret(words); found {
			return githubPostSite{rule: githubPostRuleSecret, place: place, post: true}, e.Line, true
		}
		for _, src := range githubPostBodySources(words) {
			if src.name == "-" {
				if githubPostTextsSecret(githubPostStdinTexts(execs, i)) {
					return githubPostSite{rule: githubPostRuleSecret, place: githubPostWhereCommand, post: true}, e.Line, true
				}
				continue
			}
			if content, ok := githubPostReadExecFile(src.name, e.Dir); ok {
				line, found := githubPostRawSecretLine(content)
				if !found && src.json {
					if text, ok := githubPostJSONText(content); ok {
						line, found = githubPostSecretLine(text)
					}
				}
				if found {
					return githubPostSite{rule: githubPostRuleSecret, place: src.name + ":" + strconv.Itoa(line), post: true}, e.Line, true
				}
			}
			// The file as the text leaves it: what an earlier execution of the same text writes into it from text it shows.
			if githubPostTextsSecret(githubPostWrittenTexts(execs, i, src.name)) {
				return githubPostSite{rule: githubPostRuleSecret, place: githubPostWhereCommand, post: true}, e.Line, true
			}
		}
	}
	return githubPostSite{}, 0, false
}

// githubPostBodySource is a file a gh command posts the text of; "-" is standard input. json is a request body (gh api --input).
type githubPostBodySource struct {
	name string
	json bool
}

// githubPostBodySources is every file a gh command's words name as posted text: --body-file, -F, --notes-file of a pr, issue or
// release command, and for gh api --input and the @file of a -F or --field value. A value the reader could not evaluate is left
// out.
func githubPostBodySources(words []string) []githubPostBodySource {
	if len(words) < 2 {
		return nil
	}
	api := githubPostProgram(words[1]) == "api"
	var out []githubPostBodySource
	add := func(v string, json bool) {
		if v != "" && !strings.Contains(v, githubPostUnknownMark) {
			out = append(out, githubPostBodySource{v, json})
		}
	}
	for i := 2; i < len(words); i++ {
		name, value, attached := strings.Cut(words[i], "=")
		var file, field, input bool
		switch name {
		case "--body-file", "--notes-file":
			file = !api
		case "-F":
			file, field = !api, api
		case "--field":
			field = api
		case "--input":
			input = api
		}
		if !file && !field && !input {
			continue
		}
		if !attached {
			if i+1 >= len(words) {
				break
			}
			i++
			value = words[i]
		}
		switch {
		case file:
			add(value, false)
		case input:
			add(value, true)
		case field:
			if _, v, ok := strings.Cut(value, "="); ok && strings.HasPrefix(v, "@") {
				add(strings.TrimPrefix(v, "@"), false)
			}
		}
	}
	return out
}

// githubPostReadExecFile reads a body file named in an execution's directory, under the rules of githubPostReadFileAt (a regular
// file, named without a final link, of at most 1 MiB, wherever it lies).
func githubPostReadExecFile(name string, dir shellir.Dir) (string, bool) {
	base := dir.Path
	if !dir.Known {
		base = githubPostNoDir
	}
	text, _, cause := githubPostReadFileAt(name, base)
	return text, cause == ""
}

// githubPostStdinTexts is the text execution i reads on standard input, as far as the command shows it: its here-strings and
// here-documents, a file it redirects in (read under the body file rules), and what the stages of its pipeline before it write
// (their known arguments, here-documents and files redirected in).
func githubPostStdinTexts(execs []shellir.Exec, i int) []string {
	texts := githubPostOwnInput(execs[i])
	if id := execs[i].Ctx.PipeID(); id != 0 {
		for j := 0; j < i; j++ {
			if execs[j].Ctx.PipeID() == id {
				texts = append(texts, githubPostShownText(execs[j])...)
			}
		}
	}
	return texts
}

// githubPostOwnInput is the text an execution's own input redirections show: here-strings, here-documents and the files it
// redirects in.
func githubPostOwnInput(e shellir.Exec) []string {
	var texts []string
	for _, r := range e.Redirs {
		if r.Fd != "" && r.Fd != "0" || !r.Target.Known {
			continue
		}
		switch r.Op {
		case "<<", "<<-", "<<<":
			texts = append(texts, r.Target.Value)
		case "<", "<>":
			if content, ok := githubPostReadExecFile(r.Target.Value, e.Dir); ok {
				texts = append(texts, content)
			}
		}
	}
	return texts
}

// githubPostShownText is the text an execution shows that it may write: its known arguments and what its own input redirections
// show.
func githubPostShownText(e shellir.Exec) []string {
	var texts []string
	for _, a := range e.Args {
		if a.Known {
			texts = append(texts, a.Value)
		}
	}
	return append(texts, githubPostOwnInput(e)...)
}

// githubPostWrittenTexts is the text the executions before i write into the body file name of execution i, as far as the command
// shows it: an execution that redirects its output into that file (>, >>, >|, &>, &>>) or names it as an argument of tee shows
// its own text and what it reads on standard input.
func githubPostWrittenTexts(execs []shellir.Exec, i int, name string) []string {
	target, ok := githubPostResolve(name, execs[i].Dir)
	if !ok {
		return nil
	}
	var texts []string
	for j := 0; j < i; j++ {
		e := execs[j]
		writes := false
		for _, r := range e.Redirs {
			switch r.Op {
			case ">", ">>", ">|", "&>", "&>>":
				if r.Fd == "" || r.Fd == "1" || r.Fd == "2" {
					if p, ok := githubPostResolve(r.Target.Value, e.Dir); ok && r.Target.Known && p == target {
						writes = true
					}
				}
			}
		}
		if e.Kind == shellir.KindCommand && e.Name == "tee" {
			for _, a := range e.Args {
				if p, ok := githubPostResolve(a.Value, e.Dir); ok && a.Known && p == target {
					writes = true
				}
			}
		}
		if writes {
			texts = append(texts, githubPostShownText(e)...)
			texts = append(texts, githubPostStdinTexts(execs, j)...)
		}
	}
	return texts
}

// githubPostResolve is a file name as an absolute clean path in an execution's directory; ok is false for a relative name in a
// directory the reader does not know.
func githubPostResolve(name string, dir shellir.Dir) (string, bool) {
	if name == "" || strings.Contains(name, githubPostUnknownMark) {
		return "", false
	}
	if filepath.IsAbs(name) {
		return filepath.Clean(name), true
	}
	if !dir.Known || dir.Path == "" {
		return "", false
	}
	return filepath.Join(dir.Path, name), true
}

// githubPostTextsSecret is whether any of the texts holds a secret.
func githubPostTextsSecret(texts []string) bool {
	for _, t := range texts {
		if _, found := githubPostRawSecretLine(t); found {
			return true
		}
	}
	return false
}
