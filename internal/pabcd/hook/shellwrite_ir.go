package hook

import (
	"path/filepath"
	"regexp"
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
	return shellIRDests(command, cwd, lookup, false)
}

// shellIRWriteDestsResolved is shellIRWriteDests with each relative destination joined to the directory its program runs in
// (a cd earlier in the text changes it); a destination in a directory the reader does not know is the unknown destination.
func shellIRWriteDestsResolved(command, cwd string, lookup func(string) (string, bool)) (dests []string, ok bool) {
	return shellIRDests(command, cwd, lookup, true)
}

func shellIRDests(command, cwd string, lookup func(string) (string, bool), resolve bool) (dests []string, ok bool) {
	res, err := shellir.AnalyzeEnv(command, cwd, lookup)
	if err != nil {
		return nil, false
	}
	for _, e := range res.Execs {
		if e.Kind == shellir.KindScriptFile {
			dests = append(dests, shellIRUnknownDest)
			continue
		}
		protectDir := false
		if resolve && e.Inline != nil && e.Inline.Language == "python" {
			protectDir = shellIRPyProtectedDir(e.Dir, lookup)
		}
		own := shellIRExecDestsIn(e, protectDir)
		if resolve {
			own = shellIRResolve(own, e.Dir, cwd)
		}
		dests = append(dests, own...)
	}
	return shellIRUnique(dests), true
}

// shellIRExecDests returns the destinations one program record writes: its redirections, the files its verb names and the writes
// of its inline program, as written (relative ones are not joined to a directory).
func shellIRExecDests(e shellir.Exec) []string {
	return shellIRExecDestsIn(e, false)
}

func shellIRExecDestsIn(e shellir.Exec, protectDir bool) []string {
	var own []string
	for _, r := range e.Redirs {
		if shellIRWriteRedir(r.Op) {
			own = append(own, shellIRWordDest(r.Target))
		}
	}
	own = append(own, shellIRVerbDests(e)...)
	return append(own, shellIRLanguageDests(e, protectDir)...)
}

// A computed Python destination may be relative to its effective directory.
// Reuse the memory gate's lexical and physical path reading, including symlinks.
// An unknown directory cannot establish the outside-directory control.
func shellIRPyProtectedDir(dir shellir.Dir, lookup func(string) (string, bool)) bool {
	if !dir.Known || dir.Path == "" {
		return true
	}
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	g := newMemoryGateEnv(lookup)
	root, ok := g.root()
	if !ok {
		return false
	}
	_, hit := g.hit(dir.Path, "", root)
	return hit
}

// shellIRResolve joins each relative destination to the directory its program runs in. A program in the payload's own
// directory keeps the destination as written, which the gate resolves against that directory as it always has.
func shellIRResolve(dests []string, dir shellir.Dir, cwd string) []string {
	out := make([]string, 0, len(dests))
	for _, d := range dests {
		switch {
		case d == shellIRUnknownDest || filepath.IsAbs(d) || d == "/dev/null":
			out = append(out, d)
		case dir.Known && dir.Path == cwd:
			out = append(out, d)
		case !dir.Known || dir.Path == "":
			out = append(out, shellIRUnknownDest)
		default:
			// The join is textual and keeps every component: a link in the path is followed by the filesystem, not cleaned away.
			out = append(out, strings.TrimSuffix(dir.Path, "/")+"/"+d)
		}
	}
	return out
}

// shellIRSortDests returns the file sort writes with -o, read the way getopt_long reads sort's command line: the separate,
// attached and long forms (-o FILE, -oFILE, -ro FILE, --output FILE, --output=FILE), and --output spelled as any prefix GNU
// accepts (--o, --ou, ... are --output: no other option of sort begins with o). The value of every other option that takes
// one is skipped, so a value that looks like -o is no output. An option that is no option of sort, or a prefix that is
// ambiguous, makes sort stop before it writes: no destination.
func shellIRSortDests(args []shellir.Word) []string {
	var out []string
	longs := shellir.SortLongOptions()
	for i := 0; i < len(args); i++ {
		v := shellIRPlain(args[i])
		switch {
		case v == shellIRUnknownDest:
			out = append(out, v)
		case v == "--":
			return out
		case strings.HasPrefix(v, "--"):
			name, attached, hasValue := strings.Cut(v[2:], "=")
			opt, match := shellir.MatchLongOption(longs, name)
			if match != shellir.LongFound || opt.Arg == shellir.OptionNoArg {
				continue
			}
			value, taken := attached, hasValue
			if opt.Arg == shellir.OptionRequiredArg && !hasValue {
				if i+1 < len(args) {
					i++
					value, taken = shellIRPlain(args[i]), true
					if value == shellIRUnknownDest && opt.Key != "o" {
						// The value of another option is a word the reader cannot evaluate; it may split into more words
						// (-k $K with K='1 -o FILE'), so the output file is not proven.
						out = append(out, value)
					}
				} else if opt.Key == "o" {
					out = append(out, shellIRUnknownDest)
				}
			}
			if taken && opt.Key == "o" {
				out = append(out, value)
			}
		case strings.HasPrefix(v, "-") && len(v) > 1:
			for j := 1; j < len(v); j++ {
				if !strings.ContainsRune(shellir.SortShortValueOptions, rune(v[j])) {
					continue
				}
				value := v[j+1:]
				if value == "" {
					if i+1 >= len(args) {
						if v[j] == 'o' {
							out = append(out, shellIRUnknownDest)
						}
						break
					}
					i++
					value = shellIRPlain(args[i])
					if value == shellIRUnknownDest && v[j] != 'o' {
						out = append(out, value) // may split into more words, -o among them
					}
				}
				if v[j] == 'o' {
					out = append(out, value)
				}
				break // the rest of the word, or the next word, is this option's value
			}
		}
	}
	return out
}

// shellIRUnique keeps the first of each destination.
func shellIRUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		// A write to /dev/null changes no file, so it is no destination of the gate.
		if !seen[d] && d != "/dev/null" {
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
	res, err := shellir.AnalyzeNoDir(command)
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
	if shellIRRunTimeCarrier(e.Ctx.Carrier) && shellIRWriterVerb(filepath.Base(e.Name)) {
		// xargs, find -exec and entr build the operands of this program at run time, so its destination is not in the text.
		return []string{shellIRUnknownDest}
	}
	switch filepath.Base(e.Name) {
	case "tee":
		// tee writes its operands; options end at --.
		return shellIRTeeDests(args)
	case shellir.FileRecordName:
		// The files a wrapper writes (script's transcript and log files, strace -o): every operand is a file.
		return shellIRFileDests(args)
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
		return shellIRSortDests(args)
	}
	return nil
}

// shellIRFileDests returns the operands of a wrapper's file record. None is an option, so a word that starts with a dash is
// still a file.
func shellIRFileDests(args []shellir.Word) []string {
	var out []string
	for _, a := range args {
		out = append(out, shellIRPlain(a))
	}
	return out
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
func shellIRLanguageDests(e shellir.Exec, protectDir bool) []string {
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
		res := shellVerbScriptWritesInDir(src, true, isPy, protectDir)
		if un := shellVerbUnescape(src); un != src {
			res = append(res, shellVerbScriptWritesInDir(un, true, isPy, protectDir)...)
		}
		// A writer whose destination is not a literal, and a Python open bound to a name, write to a destination the text
		// does not show: unknown (CRW-998, CRW-851).
		if shellIRDynamicWrite(src, isPy, protectDir) {
			res = append(res, shellIRUnknownDest)
		}
		return append(out, res...)
	case "sed":
		return append(out, shellVerbSedWrites(shellIRStrings(e.Args))...)
	case "perl", "ruby":
		// No reader of perl or ruby code is in this port, so the program's writes are not read: unknown.
		return append(append(out, shellVerbInterp(shellIRStrings(e.Args), false)...), shellIRUnknownDest)
	}
	// An interpreter program with no reader here (awk) is code the text shows and this port does not read: unknown.
	return append(out, shellIRUnknownDest)
}

// shellIRDynamicWrite is whether a Node or Python program calls a writer with a first argument that is no string literal,
// or binds open to a name (f = open) so that its calls are not visible as open(...).
func shellIRDynamicWrite(src string, python, protectDir bool) bool {
	if shellIRNodeDynamicWrite.MatchString(src) || shellIRStructuralWriteUnknownFrom(src, 0, python, protectDir) {
		return true
	}
	return python && shellIRPyOpenAlias.MatchString(src)
}

var (
	shellIRNodeDynamicWrite = regexp.MustCompile("\\b(?:writeFileSync|writeFile|appendFileSync|appendFile|createWriteStream)\\s*\\(\\s*[^'\"\x60\\s)]")
	shellIRPyOpenAlias      = regexp.MustCompile(`[=,(\[:]\s*open\s*(?:[,)\]:;#\r\n]|$)`)
)

// shellIRSedDests returns the files sed -i rewrites (-i, -I and --in-place in any spelling, abbreviations included). Every
// operand is reported, the script included, as the reading of sed's operands always did; the option values of -e, -f and -l
// are not operands, and neither is the empty suffix word of the BSD form of -i. A word the reader cannot evaluate is the
// unknown destination.
func shellIRSedDests(args []shellir.Word) []string {
	for _, a := range args {
		if v := shellIRPlain(a); v == shellIRUnknownDest {
			return []string{v}
		}
	}
	pa, err := shellir.ParseSedArgs("sed", args)
	if err != nil {
		return []string{shellIRUnknownDest}
	}
	if !pa.InPlace {
		return nil
	}
	var out []string
	for i, w := range pa.Operands {
		if v := shellIRPlain(w); v != "" && v != "-" {
			out = append(out, v)
			// the first operand is the script when no -e or -f gives one: no file, no backup
			if i > 0 || len(pa.Scripts)+len(pa.Files) > 0 {
				out = append(out, shellIRSedBackups(v, pa.InPlaceSuffix)...)
			}
		}
	}
	// POSIXLY_CORRECT reading: every word from the first operand on is a file, and only the -i before it counts, with its own
	// suffix (a later -i does not replace it there)
	if pa.PosixInPlace {
		for i, w := range pa.PosixTail {
			if v := shellIRPlain(w); v != "" && v != "-" {
				out = append(out, v)
				if i > 0 || len(pa.Scripts)+len(pa.Files) > 0 {
					out = append(out, shellIRSedBackups(v, pa.PosixInPlaceSuffix)...)
				}
			}
		}
	}
	return out
}

// shellIRSedBackups returns where sed -i may move the original of file when the backup suffix names a place: GNU sed replaces
// each * of the suffix by the name of the input file (its base name in older releases, the name as given in sed 4.9) and
// takes the suffix as a path, relative to the input's directory or to the working directory (releases differ), so every
// reading is reported; a suffix without * is appended to the name. A suffix that holds neither * nor / puts the backup next to
// the input and names no other place.
func shellIRSedBackups(file, suffix string) []string {
	if !strings.ContainsAny(suffix, "*/") {
		return nil
	}
	if !strings.Contains(suffix, "*") {
		return []string{file + suffix}
	}
	base := filepath.Base(file)
	out := []string{strings.ReplaceAll(suffix, "*", file)}
	if base != file {
		sub := strings.ReplaceAll(suffix, "*", base)
		out = append(out, sub)
		if !filepath.IsAbs(sub) {
			out = append(out, filepath.Join(filepath.Dir(file), sub))
		}
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

// ShellWriteDestinations is the destinations a shell command writes, read by the shared command reader with no working
// directory: a relative destination is named as written. A command the reader cannot read names the unknown destination
// alone, which a gate asks a grant for.
func ShellWriteDestinations(command string) []string {
	dests, ok := shellIRWriteDests(command, "", nil)
	if !ok {
		return []string{shellIRUnknownDest}
	}
	if dests == nil {
		return []string{}
	}
	return dests
}

// shellIRRunTimeCarrier names the carriers whose operands are made at run time: the program they run gets its operands
// from the input or from the file list, not from the text (xargs, find -exec and -execdir and -ok, entr). A parallel
// template names its sources in the text, so its command lines are read like any other text.
func shellIRRunTimeCarrier(carrier string) bool {
	return carrier == "xargs" || carrier == "find" || carrier == "entr"
}

// shellIRWriterVerb names the programs whose destination shellIRVerbDests reads.
func shellIRWriterVerb(name string) bool {
	switch name {
	case "tee", "cp", "mv", "install", "dd", "sed", "sort":
		return true
	}
	return false
}
