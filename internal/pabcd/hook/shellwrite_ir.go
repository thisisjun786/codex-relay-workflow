package hook

import (
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// shellIRUnknownDest marks a write whose destination the reader cannot evaluate. The memory gate treats it as a
// write to an unknown destination, which needs a grant.
const shellIRUnknownDest = "\x00unknown"

// shellIRWriteDests returns the destinations of the writes a command makes, read from the reader's program records.
// A destination the reader cannot evaluate is returned as shellIRUnknownDest. ok is false when the reader cannot
// read the command at all.
func shellIRWriteDests(command, cwd string, lookup func(string) (string, bool)) (dests []string, ok bool) {
	res, err := shellir.AnalyzeEnv(command, cwd, lookup)
	if err != nil {
		return nil, false
	}
	for _, e := range res.Execs {
		if e.Kind == shellir.KindScriptFile {
			dests = append(dests, shellIRUnknownDest)
			continue
		}
		for _, r := range e.Redirs {
			if shellIRWriteRedir(r.Op) {
				dests = append(dests, shellIRWordDest(r.Target))
			}
		}
		dests = append(dests, shellIRVerbDests(e)...)
		dests = append(dests, shellIRLanguageDests(e)...)
	}
	return shellIRUnique(dests), true
}

// shellIRUnique keeps the first of each destination.
func shellIRUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// shellIRWriteRedir is whether a redirection operator writes its target.
func shellIRWriteRedir(op string) bool {
	switch op {
	case ">", ">>", ">|", ">>|", "<>", "&>", "&>>", "&>|", "&>>|":
		return true
	}
	return false
}

func shellIRWordDest(w shellir.Word) string {
	if !w.Known || strings.Contains(w.Value, "%") {
		return shellIRUnknownDest
	}
	return w.Value
}

// shellIRPlain is a known word's value, or the unknown mark for a word the reader cannot evaluate.
func shellIRPlain(w shellir.Word) string { return shellIRWordDest(w) }

// shellIRStrings is the values of a word list, with the unknown mark for each word the reader cannot evaluate.
func shellIRStrings(ws []shellir.Word) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, shellIRPlain(w))
	}
	return out
}

// shellIRFStringUnreadable reports the first Python program of a command that holds an f-string the reader cannot walk.
// The programs come from the shared reader's records, so a program a nested shell -c or eval runs is included. A command
// the reader cannot read is itself unreadable.
func shellIRFStringUnreadable(command string) (string, bool) {
	res, err := shellir.Analyze(command, "")
	if err != nil {
		return "the command reader refused it", true
	}
	for _, e := range res.Execs {
		if e.Inline == nil || e.Inline.Language != "python" || !e.Inline.Source.Known {
			continue
		}
		if what, bad := shellIRProgramUnreadable(e.Inline.Source.Value); bad {
			return what, true
		}
	}
	return "", false
}

// shellIRInPlaceFiles reads the file operands of a perl or ruby program run with -i, which edits them in place: every
// operand that is not an option is a file the program may write.
func shellIRInPlaceFiles(args []shellir.Word) []string {
	inPlace := false
	for _, a := range args {
		v := shellIRPlain(a)
		if v == shellIRUnknownDest {
			return []string{v}
		}
		if strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "--") && strings.Contains(v, "i") {
			inPlace = true
		}
	}
	if !inPlace {
		return nil
	}
	var files []string
	for _, a := range args {
		if v := shellIRPlain(a); !strings.HasPrefix(v, "-") {
			files = append(files, v)
		}
	}
	return files
}

// shellIRVerbDests reads the files a file-writing program names. tee's operands are files (options end at --). cp, mv
// and install write to their -t directory (in a cluster, attached, or --target-directory=) or else to their last
// operand, skipping a --suffix value. dd writes its of=, sort writes -o, and sed -i writes its file operands.
func shellIRVerbDests(e shellir.Exec) []string {
	args := e.Args
	switch filepath.Base(e.Name) {
	case "tee":
		return shellIRTeeDests(args)
	case "cp", "mv", "install":
		return shellIRCopyDests(args)
	case "dd":
		var out []string
		for _, a := range args {
			v := shellIRPlain(a)
			if v == shellIRUnknownDest {
				out = append(out, v)
			} else if strings.HasPrefix(v, "of=") {
				out = append(out, strings.TrimPrefix(v, "of="))
			}
		}
		return out
	case "sed":
		return append(shellIRSedScriptDests(args), shellIRSedDests(args)...)
	case "sort":
		for i, a := range args {
			if shellIRPlain(a) == "-o" && i+1 < len(args) {
				return []string{shellIRPlain(args[i+1])}
			}
		}
	}
	return nil
}

// shellIRTeeDests returns tee's file operands: every word after the options, and the words of a -- ends the options.
func shellIRTeeDests(args []shellir.Word) []string {
	var out []string
	optsDone := false
	for _, a := range args {
		v := shellIRPlain(a)
		switch {
		case v == shellIRUnknownDest:
			out = append(out, v)
		case !optsDone && v == "--":
			optsDone = true
		case !optsDone && strings.HasPrefix(v, "-") && v != "-":
		default:
			out = append(out, v)
		}
	}
	return out
}

// shellIRLanguageDests reads the writes of an interpreter's program with the language readers. perl and ruby with -i also
// edit their file operands, whether or not the program is inline. An inline program with no reader is unknown.
func shellIRLanguageDests(e shellir.Exec) []string {
	var out []string
	if n := filepath.Base(e.Name); n == "perl" || n == "ruby" {
		out = append(out, shellIRInPlaceFiles(e.Args)...)
	}
	if e.Inline == nil {
		return out
	}
	src := e.Inline.Source.Value
	if !e.Inline.Source.Known {
		return append(out, shellIRUnknownDest)
	}
	switch e.Inline.Language {
	case "python", "node":
		isPy := e.Inline.Language == "python"
		res := shellVerbScriptWritesIn(src, true, isPy)
		if un := shellVerbUnescape(src); un != src {
			res = append(res, shellVerbScriptWritesIn(un, true, isPy)...)
		}
		return append(out, res...)
	case "sed":
		return append(out, shellVerbSedWrites(shellIRStrings(e.Args))...)
	case "perl", "ruby":
		return append(out, shellVerbInterp(shellIRStrings(e.Args), false)...)
	}
	// An interpreter program with no reader here (awk) is code the text shows and this port does not read: unknown.
	return append(out, shellIRUnknownDest)
}

// shellIRSedDests returns the files sed -i rewrites. Every operand is reported, the script included, as the reading of
// sed's operands always did; the option values of -e, -f and -l are not operands.
func shellIRSedDests(args []shellir.Word) []string {
	inPlace := false
	for _, a := range args {
		v := shellIRPlain(a)
		if v == shellIRUnknownDest {
			return []string{v}
		}
		if v == "--in-place" || strings.HasPrefix(v, "--in-place=") || (strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "--") && strings.Contains(v, "i")) {
			inPlace = true
		}
	}
	if !inPlace {
		return nil
	}
	var out []string
	skip := false
	for i, a := range args {
		v := shellIRPlain(a)
		if skip {
			skip = false
			continue
		}
		if v == "-i" && i+1 < len(args) && shellIRPlain(args[i+1]) == "" {
			skip = true
			continue
		}
		if v == "-e" || v == "-f" || v == "-l" {
			skip = true
			continue
		}
		if strings.HasPrefix(v, "-") && v != shellIRUnknownDest {
			continue
		}
		out = append(out, v)
	}
	return out
}

// shellIRCopyDests returns the destinations of cp, mv or install: the -t directory with every operand after it, or else
// the last operand before any --suffix value. A --suffix value is reported too, as the reading of the operands always did.
func shellIRCopyDests(args []shellir.Word) []string {
	var target string
	haveTarget := false
	var operands, suffixes []string
	optsDone := false
	for i := 0; i < len(args); i++ {
		v := shellIRPlain(args[i])
		switch {
		case v == shellIRUnknownDest:
			operands = append(operands, v)
		case !optsDone && v == "--":
			optsDone = true
		case !optsDone && (v == "-t" || v == "--target-directory"):
			if i+1 >= len(args) {
				return []string{shellIRUnknownDest}
			}
			target, haveTarget = shellIRPlain(args[i+1]), true
			i++
		case !optsDone && strings.HasPrefix(v, "--target-directory="):
			if t := strings.TrimPrefix(v, "--target-directory="); t != "" {
				target, haveTarget = t, true
			}
		case !optsDone && (v == "-S" || v == "--suffix"):
			if i+1 < len(args) {
				suffixes = append(suffixes, shellIRPlain(args[i+1]))
			}
			i++
		case !optsDone && strings.HasPrefix(v, "--suffix="):
			suffixes = append(suffixes, strings.TrimPrefix(v, "--suffix="))
		case !optsDone && strings.HasPrefix(v, "--"):
		case !optsDone && strings.HasPrefix(v, "-") && v != "-":
			// a cluster: -t takes the rest of the word or the next word; -S takes the rest of the word or the next word.
			for k := 1; k < len(v); k++ {
				c := v[k]
				if c == 't' {
					if k == len(v)-1 {
						if i+1 >= len(args) {
							return []string{shellIRUnknownDest}
						}
						target, haveTarget = shellIRPlain(args[i+1]), true
						i++
					} else {
						target, haveTarget = v[k+1:], true
					}
					break
				}
				if c == 'S' {
					if k == len(v)-1 {
						if i+1 < len(args) {
							suffixes = append(suffixes, shellIRPlain(args[i+1]))
						}
						i++
					} else {
						suffixes = append(suffixes, v[k+1:])
					}
					break
				}
			}
		default:
			operands = append(operands, v)
		}
	}
	if haveTarget {
		return append(append([]string{target}, operands...), suffixes...)
	}
	var out []string
	if len(operands) > 0 {
		out = append(out, operands[len(operands)-1])
	}
	return append(out, suffixes...)
}

// shellIRProgramUnreadable reports the what of a Python program the walk cannot finish, read from the exact bytes the
// shell handed over: an f-string replacement field it cannot read, or a program passed to exec, eval or compile whose
// first argument is no string literal. The shell has already removed its own quoting, so no unescaped second reading
// is taken here.
func shellIRProgramUnreadable(program string) (string, bool) {
	if what, bad := shellWriteFStringProgramUnreadable(program); bad {
		return what, true
	}
	if _, what := shellWriteExecScan(shellVerbWithoutComments(program, true), true, 0); what != "" {
		return what, true
	}
	return "", false
}
