package shellir

import (
	"slices"
	"strings"
)

// shellFlagLetters are the single-letter options a shell accepts before its
// program. A letter outside this set is refused.
const shellFlagLetters = "abefhiklmnprtuvxBCDEHPTs"

func shellLongOption(v string) bool {
	switch v {
	case "--login", "--norc", "--noprofile", "--posix", "--restricted", "--verbose", "--noediting":
		return true
	}
	return false
}

// shellCall reads a shell invocation: its -c string, its script file operand,
// or the here-document or here-string it reads from standard input.

// suCall reads su -c, the only form whose program is visible: the command
// string runs under a shell.
func (w *walker) suCall(args []Word, st *state, ctx Context) error {
	var code *Word
	for i := 0; i < len(args); i++ {
		v, err := knownValue(args[i], "su option")
		if err != nil {
			return err
		}
		switch {
		case v == "-" || v == "-l" || v == "--login":
		case v == "-c":
			if i+1 >= len(args) {
				return unreadablef("su -c without a command")
			}
			code = &args[i+1]
			i++
		case strings.HasPrefix(v, "--command="):
			c := Word{Known: true, Value: strings.TrimPrefix(v, "--command=")}
			code = &c
		default:
			return unreadablef("su argument %q is not modelled", v)
		}
	}
	if code == nil {
		return unreadablef("su without -c starts an interactive shell")
	}
	text, err := knownValue(*code, "su -c command")
	if err != nil {
		return err
	}
	return w.carried(text, st.clone(), ctx, "su -c")
}

type interpSpec struct {
	code      string   // letters whose argument is program text
	consume   string   // letters whose argument is not program text
	flags     string   // letters with no argument
	attach    bool     // a code letter may carry its program in the same word
	longFlags []string // long options that take no value
}

func interpreterSpec(lang string) interpSpec {
	switch lang {
	case "python":
		return interpSpec{code: "c", consume: "WX", flags: "BbdEhiIOPqRsSuvxV", attach: true}
	case "node":
		return interpSpec{code: "ep", consume: "r", flags: "ci", longFlags: nodeLongFlags}
	case "perl":
		return interpSpec{code: "eE", flags: "wWXnpsTtUcSaFlvi", attach: true}
	case "ruby":
		return interpSpec{code: "e", consume: "CI", flags: "wWdnpsyvcUalFrhxTi", attach: true}
	}
	return interpSpec{}
}

// interpreterInline returns the program an interpreter runs when the text
// shows it. A script file operand is not judged here, except for awk -f and
// sed -f, whose file the caller reads as a script record.
func (w *walker) interpreterInline(name string, args []Word, redirs []Redir, ctx Context) (*Inline, *Word, error) {
	lang := interpreterLanguage(name)
	switch lang {
	case "awk":
		return awkInline(name, args)
	case "sed":
		return sedInline(name, args, redirs, ctx)
	}
	codes, operand, err := clusterInterp(name, args, interpreterSpec(lang))
	if err != nil {
		return nil, nil, err
	}
	if len(codes) > 1 {
		return nil, nil, unreadablef("%s receives more than one program", name)
	}
	if len(codes) == 1 {
		text, err := knownValue(codes[0], name+" program")
		if err != nil {
			return nil, nil, err
		}
		return &Inline{Language: lang, Source: Word{Known: true, Value: text}}, nil, nil
	}
	if operand < len(args) && args[operand].Value != "-" {
		if args[operand].Known && fdAliasPath(args[operand].Value) {
			// A script operand named through a descriptor alias runs the text that descriptor carries: a here-document,
			// a here-string, or the pipe on standard input. An alias the text does not set is unreadable.
			text, err := aliasProgram(args[operand].Value, redirs, ctx, name)
			if err != nil {
				return nil, nil, err
			}
			return &Inline{Language: lang, Source: Word{Known: true, Value: text}}, nil, nil
		}
		return nil, nil, nil
	}
	if _, ok := stdinIsFile(ctx); ok {
		// The program is a file the text names (python3 < prog.py): run like a script file operand, not judged.
		return nil, nil, nil
	}
	text, err := stdinProgram(redirs, ctx.Stdin, name, ctx)
	if err != nil {
		return nil, nil, err
	}
	return &Inline{Language: lang, Source: Word{Known: true, Value: text}}, nil, nil
}

// nodeLongFlags are the long options of node that take no value; any other long option may take one, which would move the
// script operand, so it stays unreadable.
var nodeLongFlags = []string{"--no-warnings", "--no-deprecation", "--trace-warnings", "--trace-deprecation", "--throw-deprecation",
	"--pending-deprecation", "--enable-source-maps", "--use-strict", "--preserve-symlinks", "--expose-gc", "--harmony"}

// lastStdinFile is the file the last input redirection of a statement names, when it is a file the reader knows and not a
// descriptor alias.
func lastStdinFile(redirs []Redir) (string, bool) {
	for i := len(redirs) - 1; i >= 0; i-- {
		r := redirs[i]
		if r.Fd != "" && r.Fd != "0" {
			continue
		}
		switch r.Op {
		case "<":
			if r.Target.Known && !fdAliasPath(r.Target.Value) {
				return r.Target.Value, true
			}
			return "", false
		case "<<", "<<-", "<<<", "<&", "<>":
			return "", false
		case ">&", ">", ">>", ">|", "&>":
			if r.Fd == "0" {
				return "", false // 0>&3 copies a descriptor onto standard input
			}
		}
	}
	return "", false
}

// stdinIsFile reports that the command reads a file the text names (a redirection of its own, or the one a shell around it set,
// as in bash -c 'bash' </dev/null) and is no right side of a pipe in its own text: zsh with MULTIOS reads the pipe as well as
// the file there. It returns the file.
func stdinIsFile(ctx Context) (string, bool) {
	if ctx.Stdin != StdinFile || ctx.inTextPipe || ctx.stdinFile == "" {
		return "", false
	}
	return ctx.stdinFile, true
}

// isOpaqueInterpreter names the interpreters of other languages the port has no reader for.
func isOpaqueInterpreter(name string) bool {
	switch strings.ToLower(name) {
	case "php", "lua", "luajit", "rscript", "tclsh", "wish", "osascript", "groovy":
		return true
	}
	return false
}

// opaqueGrammar is the closed option grammar of an interpreter the port cannot read: the options that take no value, the
// options that take one (a separate word, or attached for a single-dash letter), and the options whose value is the script
// file. Every other option is outside the model; a code option (-e, -r, --eval) is among them, so no spelling of it (attached,
// --name=value) reaches the script operand.
type opaqueGrammar struct {
	flags  []string
	valued []string
	script []string
}

var opaqueGrammars = map[string]opaqueGrammar{
	"php":       {flags: []string{"-n", "-q", "-C", "-H"}, valued: []string{"-c", "-d"}, script: []string{"-f"}},
	"lua":       {flags: []string{"-E", "-W"}},
	"luajit":    {},
	"rscript":   {flags: []string{"--vanilla", "--no-save", "--no-restore", "--no-site-file", "--no-init-file", "--no-environ", "--slave", "--quiet", "--silent", "--verbose"}},
	"tclsh":     {valued: []string{"-encoding"}},
	"wish":      {valued: []string{"-encoding", "-display", "-geometry", "-name", "-visual", "-use", "-colormap"}, script: []string{"-file"}},
	"osascript": {valued: []string{"-l", "-s"}},
	"groovy":    {valued: []string{"-c", "-cp", "-classpath", "--classpath", "--encoding"}},
}

func opaqueHas(set []string, v string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// opaqueInterpreter refuses the program positions of an interpreter the port cannot read: a program on standard input, in a
// descriptor alias, or in a code option. The options are read by the interpreter's own grammar (opaqueGrammars): an option
// value is no script file, and an option outside the grammar is unreadable. A script file operand is not judged, as for the
// interpreters the port reads.
func opaqueInterpreter(name string, args []Word, redirs []Redir, ctx Context) error {
	if len(args) == 1 && args[0].Known {
		switch args[0].Value {
		case "--version", "-v", "-V", "--help", "-h":
			return nil
		}
	}
	grammar := opaqueGrammars[strings.ToLower(name)]
	operand := false
options:
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.Known {
			operand = true // a word the reader cannot read stands for the script (a residual shared with every interpreter)
			break
		}
		v := a.Value
		switch {
		case v == "-" || fdAliasPath(v):
			return unreadablef("%s reads its program from standard input or a descriptor alias (%s)", name, v)
		case v == "--":
			// the word after -- is the script operand, which is judged as one before the options end (php -- /dev/stdin)
			if i+1 < len(args) {
				operand = true
				if next := args[i+1]; next.Known && (next.Value == "-" || fdAliasPath(next.Value)) {
					return unreadablef("%s reads its program from standard input or a descriptor alias (%s)", name, next.Value)
				}
			}
			break options
		case !strings.HasPrefix(v, "-"):
			operand = true
			break options
		case opaqueHas(grammar.flags, v):
		case opaqueHas(grammar.valued, v), opaqueHas(grammar.script, v):
			if i+1 >= len(args) {
				return unreadablef("%s option %s has no value", name, v)
			}
			i++
			if !args[i].Known {
				return unreadablef("%s option %s has a value that is not known (%s)", name, v, args[i].Reason)
			}
			if opaqueHas(grammar.script, v) {
				if w := args[i].Value; w == "-" || fdAliasPath(w) {
					return unreadablef("%s reads its program from standard input or a descriptor alias (%s)", name, w)
				}
				operand = true
				break options
			}
		case len(v) > 2 && v[0] == '-' && v[1] != '-' && opaqueHas(grammar.valued, v[:2]):
			// an attached value (-dname=value)
		default:
			return unreadablef("%s option %s is outside the options the port models (a program may be in it)", name, v)
		}
	}
	if _, file := stdinIsFile(ctx); !operand && !file {
		return unreadablef("%s has no script file, so it reads its program from standard input", name)
	}
	return nil
}

func awkInline(name string, args []Word) (*Inline, *Word, error) {
	i := 0
	for i < len(args) {
		v, err := knownValue(args[i], name+" option")
		if err != nil {
			return nil, nil, err
		}
		if v == "--" {
			i++
			break
		}
		if v == "-" || !strings.HasPrefix(v, "-") {
			break
		}
		switch {
		case v == "-F" || v == "-v":
			i += 2
		case strings.HasPrefix(v, "-F") || strings.HasPrefix(v, "-v"):
			i++
		case v == "-f":
			if i+1 >= len(args) {
				return nil, nil, unreadablef("%s -f without a file", name)
			}
			file := args[i+1]
			return nil, &file, nil
		case strings.HasPrefix(v, "-f"):
			file := Word{Known: true, Value: v[2:]}
			return nil, &file, nil
		default:
			return nil, nil, unreadablef("%s option %s is not modelled", name, v)
		}
	}
	if i >= len(args) {
		return nil, nil, unreadablef("%s without a program", name)
	}
	if !args[i].Known {
		return nil, nil, unreadablef("%s program is not known (%s)", name, args[i].Reason)
	}
	return &Inline{Language: "awk", Source: args[i]}, nil, nil
}

// sedInline reads sed's command line (ParseSedArgs reads its options the way getopt_long does, abbreviations included) and
// returns the script text it runs: the -e and --expression values joined by newlines, or the first operand when no -e or -f
// gives one. When a script option follows the first operand, that operand is judged as a script too (POSIXLY_CORRECT stops
// getopt there and runs it). A script file (-f, --file, in any spelling) is not read: its text is not in the command, so the
// command is unreadable, whether or not the file exists.
func sedInline(name string, args []Word, redirs []Redir, ctx Context) (*Inline, *Word, error) {
	pa, err := ParseSedArgs(name, args)
	if err != nil {
		return nil, nil, err
	}
	if len(pa.Files) > 0 {
		return nil, nil, unreadablef("%s reads its script from a file, which is not read", name)
	}
	var sources []Word
	sources = append(sources, pa.Scripts...)
	if pa.PosixScript != nil {
		sources = append(sources, *pa.PosixScript)
	}
	if len(sources) == 0 {
		if len(pa.Operands) == 0 {
			return nil, nil, unreadablef("%s without a script", name)
		}
		sources = append(sources, pa.Operands[0])
	}
	parts := make([]string, 0, len(sources))
	for _, c := range sources {
		text, err := knownValue(c, name+" script")
		if err != nil {
			return nil, nil, err
		}
		parts = append(parts, text)
	}
	return &Inline{Language: "sed", Source: Word{Known: true, Value: strings.Join(parts, "\n")}}, nil, nil
}

func isPythonName(name string) bool {
	if name == "py" {
		return true
	}
	if !strings.HasPrefix(name, "python") {
		return false
	}
	for _, c := range name[len("python"):] {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

func isInterpreter(name string) bool { return interpreterLanguage(name) != "" }

// shellCall reads a shell invocation. Options keep being read after -c, so the -c string is the first operand
// after the options, not the word that follows -c. The operand is a script file unless -c is set; without an
// operand the program comes from standard input.
func (w *walker) shellCall(name string, args []Word, redirs []Redir, st *state, ctx Context) error {
	cmdMode := false
	// readStdin: -s or a lone - makes the program standard input, so the operands after it are positional parameters.
	readStdin := false
	i := 0
loop:
	for i < len(args) {
		v, err := knownValue(args[i], name+" option")
		if err != nil {
			return err
		}
		switch {
		case v == "--" || v == "-":
			readStdin = readStdin || v == "-"
			i++
			break loop
		case strings.HasPrefix(v, "--"):
			if !shellLongOption(v) {
				return unreadablef("%s option %s is not modelled", name, v)
			}
			i++
		case len(v) > 1 && (v[0] == '-' || v[0] == '+'):
			i++
			for k := 1; k < len(v); k++ {
				c := v[k]
				if c == 'c' {
					cmdMode = true
					continue
				}
				if c == 's' {
					readStdin = true
				}
				if c == 'o' || c == 'O' {
					i++
					break
				}
				if strings.IndexByte(shellFlagLetters, c) < 0 {
					return unreadablef("%s option -%c is not modelled", name, c)
				}
			}
		default:
			break loop
		}
	}
	var operand *Word
	if i < len(args) && !readStdin {
		operand = &args[i]
	}
	if cmdMode {
		if operand == nil {
			return unreadablef("%s -c without a string", name)
		}
		text, err := knownValue(*operand, name+" -c string")
		if err != nil {
			return err
		}
		return w.carried(text, st.clone(), ctx, name+" -c")
	}
	if operand != nil && operand.Known && fdAliasPath(operand.Value) {
		// A shell that runs a descriptor alias runs the text that descriptor carries, as a shell string does.
		text, err := aliasProgram(operand.Value, redirs, ctx, name)
		if err != nil {
			return err
		}
		return w.carried(text, st.clone(), ctx, name+" fd")
	}
	if operand != nil {
		return w.scriptFile(name, *operand, st, ctx)
	}
	if file, ok := stdinIsFile(ctx); ok && ctx.Carrier != "" {
		// A shell in a carried text reads its program from a file the text names: /dev/null holds none, any other file is
		// a script file. In the top-level text the shell stays unreadable (the corpus keeps cat <<EOF; bash </dev/null refused).
		if file == "/dev/null" {
			return nil
		}
		return w.scriptFile(name, Word{Known: true, Value: file}, st, ctx)
	}
	body, err := stdinProgram(redirs, ctx.Stdin, name, ctx)
	if err != nil {
		return err
	}
	return w.carried(body, st.clone(), ctx, name+" stdin")
}

// interpreterLanguage names the interpreter a program is: the lower-case base name, without a Windows extension.
func interpreterLanguage(name string) string {
	name = strings.ToLower(name)
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		name = strings.TrimSuffix(name, ext)
	}
	switch {
	case isPythonName(name):
		return "python"
	case name == "node" || name == "nodejs":
		return "node"
	case name == "perl" || name == "ruby":
		return name
	case name == "awk" || name == "gawk" || name == "mawk" || name == "nawk":
		return "awk"
	case name == "sed":
		return "sed"
	}
	return ""
}

// clusterInterp reads interpreter options. It returns the program words it found and the index of the first operand.
// Node's --eval and --print take the program as the next word or after an =.
func clusterInterp(name string, args []Word, spec interpSpec) ([]Word, int, error) {
	var codes []Word
	i := 0
	for i < len(args) {
		v, err := knownValue(args[i], name+" option")
		if err != nil {
			return nil, 0, err
		}
		if v == "--" {
			i++
			break
		}
		if v == "--eval" || v == "--print" {
			if i+1 >= len(args) {
				return nil, 0, unreadablef("%s %s without a program", name, v)
			}
			codes = append(codes, args[i+1])
			i += 2
			continue
		}
		if strings.HasPrefix(v, "--eval=") {
			codes = append(codes, Word{Known: true, Value: strings.TrimPrefix(v, "--eval=")})
			i++
			continue
		}
		if strings.HasPrefix(v, "--") {
			if slices.Contains(spec.longFlags, v) {
				i++
				continue
			}
			return nil, 0, unreadablef("%s option %s is not modelled", name, v)
		}
		if v == "-" || len(v) < 2 || v[0] != '-' {
			break
		}
		i++
		for k := 1; k < len(v); k++ {
			c := v[k]
			last := k == len(v)-1
			switch {
			case strings.IndexByte(spec.code, c) >= 0:
				if !last && !spec.attach {
					return nil, 0, unreadablef("%s option -%c must stand alone", name, c)
				}
				if !last {
					codes = append(codes, Word{Known: true, Value: v[k+1:]})
				} else {
					if i >= len(args) {
						return nil, 0, unreadablef("%s -%c without a program", name, c)
					}
					codes = append(codes, args[i])
					i++
				}
				k = len(v)
			case strings.IndexByte(spec.consume, c) >= 0:
				if last {
					if i >= len(args) {
						return nil, 0, unreadablef("%s -%c without a value", name, c)
					}
					i++
				}
				k = len(v)
			case strings.IndexByte(spec.flags, c) >= 0:
			default:
				return nil, 0, unreadablef("%s option -%c is not modelled", name, c)
			}
		}
	}
	return codes, i, nil
}

// fdAliasPath reports a path that names a file descriptor of this process, so that reading it reads what the shell gave
// that descriptor: /dev/stdin, /dev/fd/N and /proc/<pid>/fd/N (and /proc/self/fd/N).
func fdAliasPath(p string) bool {
	switch {
	case p == "/dev/stdin", strings.HasPrefix(p, "/dev/fd/"):
		return true
	case strings.HasPrefix(p, "/proc/") && strings.Contains(p, "/fd/"):
		return true
	}
	return false
}
