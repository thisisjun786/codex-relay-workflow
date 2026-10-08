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

// shellIRLanguageDests reads the writes of an interpreter's inline program with the language readers, from the
// program text the reader extracted. awk has no reader in this port yet; its inline program is not judged here.
func shellIRLanguageDests(e shellir.Exec) []string {
	if e.Inline == nil {
		return nil
	}
	src := e.Inline.Source.Value
	if !e.Inline.Source.Known {
		return []string{shellIRUnknownDest}
	}
	switch e.Inline.Language {
	case "python", "node":
		isPy := e.Inline.Language == "python"
		out := shellVerbScriptWritesIn(src, true, isPy)
		if un := shellVerbUnescape(src); un != src {
			out = append(out, shellVerbScriptWritesIn(un, true, isPy)...)
		}
		return out
	case "sed":
		return shellVerbSedWrites(shellIRStrings(e.Args))
	case "perl", "ruby":
		return append(shellVerbInterp(shellIRStrings(e.Args), false), shellIRInPlaceFiles(e.Args)...)
	}
	return nil
}

// shellIRStrings is the values of a word list, with the unknown mark for each word the reader cannot evaluate.
func shellIRStrings(ws []shellir.Word) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, shellIRPlain(w))
	}
	return out
}

// shellIRVerbDests reads the files a file-writing program names: tee's operands, the destination of cp, mv and install
// (the -t directory when given, else the last operand), dd's of=, sort -o, and sed -i's file operands.
func shellIRVerbDests(e shellir.Exec) []string {
	args := e.Args
	switch filepath.Base(e.Name) {
	case "tee":
		var out []string
		for _, a := range args {
			if v := shellIRPlain(a); v == shellIRUnknownDest || !strings.HasPrefix(v, "-") {
				out = append(out, v)
			}
		}
		return out
	case "cp", "mv", "install":
		for i, a := range args {
			v := shellIRPlain(a)
			if v == "-t" || v == "--target-directory" {
				if i+1 < len(args) {
					return []string{shellIRPlain(args[i+1])}
				}
				return []string{shellIRUnknownDest}
			}
			if strings.HasPrefix(v, "--target-directory=") {
				return []string{strings.TrimPrefix(v, "--target-directory=")}
			}
		}
		if len(args) == 0 {
			return nil
		}
		return []string{shellIRPlain(args[len(args)-1])}
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
		return shellIRSedDests(args)
	case "sort":
		for i, a := range args {
			if shellIRPlain(a) == "-o" && i+1 < len(args) {
				return []string{shellIRPlain(args[i+1])}
			}
		}
	}
	return nil
}

// shellIRSedDests returns the files sed -i rewrites. A BSD suffix given as its own empty word is skipped. When the script
// comes from -e or -f, every operand is a file; otherwise the first operand is the script.
func shellIRSedDests(args []shellir.Word) []string {
	inPlace, scriptOpt := false, false
	for _, a := range args {
		v := shellIRPlain(a)
		if v == shellIRUnknownDest {
			return []string{v}
		}
		switch {
		case v == "--in-place" || strings.HasPrefix(v, "--in-place="):
			inPlace = true
		case strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "--") && strings.Contains(v, "i"):
			inPlace = true
		case v == "-e" || v == "-f" || strings.HasPrefix(v, "--expression") || strings.HasPrefix(v, "--file"):
			scriptOpt = true
		}
	}
	if !inPlace {
		return nil
	}
	var files []string
	operands := 0
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
		operands++
		if operands == 1 && !scriptOpt {
			continue
		}
		files = append(files, v)
	}
	return files
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
		if what, bad := shellWriteFStringUnreadableProgram(e.Inline.Source.Value); bad {
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
