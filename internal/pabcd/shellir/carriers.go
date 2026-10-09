package shellir

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// shellFlagLetters are the single-letter options a shell accepts before its
// program. A letter outside this set is refused.
const shellFlagLetters = "abefhiklmnprtuvxBCDEHPT"

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
	module    string   // letters whose argument is a module the interpreter runs (python -m); the words after it are the module's
	repl      string   // letters that keep the interpreter reading commands from standard input after its program (python -i, node -i)
	code      string   // letters whose argument is program text
	consume   string   // letters whose argument is not program text
	flags     string   // letters with no argument
	attach    bool     // a code letter may carry its program in the same word
	longFlags []string // long options that take no value
}

func interpreterSpec(lang string) interpSpec {
	switch lang {
	case "python":
		return interpSpec{module: "m", repl: "i", code: "c", consume: "WX", flags: "BbdEhiIOPqRsSuvxV", attach: true}
	case "node":
		return interpSpec{repl: "i", code: "ep", consume: "r", flags: "ci", longFlags: nodeLongFlags}
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
func (w *walker) interpreterInline(name string, args []Word, redirs []Redir, dir Dir, ctx Context) (*Inline, *Word, error) {
	lang := interpreterLanguage(name)
	switch lang {
	case "awk":
		return awkInline(name, args)
	case "sed":
		return sedInline(name, args, redirs, ctx)
	}
	codes, operand, module, repl, err := clusterInterp(name, args, interpreterSpec(lang))
	if err != nil {
		return nil, nil, err
	}
	if repl && stdinCarriesProgram(ctx.Stdin) {
		// -i keeps the interpreter reading commands from standard input after its script or program, so the pipe (or the
		// here-document) is a program whatever the script operand or the -c string shows.
		return nil, nil, unreadablef("%s -i reads commands from standard input (%s) after its program", name, ctx.Stdin)
	}
	if module != nil {
		if len(codes) > 0 {
			return nil, nil, unreadablef("%s receives a program and a module", name)
		}
		return nil, nil, w.checkPythonModule(name, *module, args[operand:], dir)
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
		// A script operand named through a file-descriptor alias runs the text of that descriptor (a here-string or a
		// pipe the text shows): the reader cannot follow the alias, so the program is unreadable.
		if args[operand].Known && fdAliasPath(args[operand].Value, dir) {
			return nil, nil, unreadablef("%s reads its program from %s, a file-descriptor alias", name, args[operand].Value)
		}
		return nil, nil, nil
	}
	if _, ok := stdinIsFile(ctx); ok {
		// The program is a file the text names (python3 < prog.py): run like a script file operand, not judged.
		return nil, nil, nil
	}
	text, err := stdinProgram(redirs, ctx.Stdin, name)
	if err != nil {
		return nil, nil, err
	}
	return &Inline{Language: lang, Source: Word{Known: true, Value: text}}, nil, nil
}

// stdinCarriesProgram is whether standard input is something a program could be read from that the text does not name as a file.
func stdinCarriesProgram(stdin string) bool {
	switch stdin {
	case StdinPipe, StdinHeredoc, StdinHerestring, StdinUnknown:
		return true
	}
	return false
}

// nodeLongFlags are the long options of node that take no value; any other long option may take one, which would move the
// script operand, so it stays unreadable.
var nodeLongFlags = []string{"--no-warnings", "--no-deprecation", "--trace-warnings", "--trace-deprecation", "--throw-deprecation",
	"--pending-deprecation", "--enable-source-maps", "--use-strict", "--preserve-symlinks", "--expose-gc", "--harmony"}

// lastStdinFile is the file the last input redirection of a statement names, when it is a file the reader knows and not a
// descriptor alias.
func lastStdinFile(redirs []Redir, dir Dir) (string, bool) {
	for i := len(redirs) - 1; i >= 0; i-- {
		r := redirs[i]
		if r.Fd != "" && r.Fd != "0" {
			continue
		}
		switch r.Op {
		case "<":
			if r.Target.Known && !fdAliasPath(r.Target.Value, dir) {
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
func opaqueInterpreter(name string, args []Word, redirs []Redir, dir Dir, ctx Context) error {
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
			if stdinCarriesProgram(ctx.Stdin) {
				// the word may name a descriptor alias, and then the interpreter runs what the pipe or the here-document carries
				return unreadablef("%s has a word that is not known (%s) and standard input is %s", name, a.Reason, ctx.Stdin)
			}
			operand = true // a word the reader cannot read stands for the script (a residual of every interpreter with no pipe)
			break
		}
		v := a.Value
		switch {
		case v == "-" || fdAliasPath(v, dir):
			return unreadablef("%s reads its program from standard input or a descriptor alias (%s)", name, v)
		case v == "--":
			// the word after -- is the script operand, which is judged as one before the options end (php -- /dev/stdin)
			if i+1 < len(args) {
				operand = true
				if next := args[i+1]; next.Known && (next.Value == "-" || fdAliasPath(next.Value, dir)) {
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
				if w := args[i].Value; w == "-" || fdAliasPath(w, dir) {
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

func sedInline(name string, args []Word, redirs []Redir, ctx Context) (*Inline, *Word, error) {
	var codes []Word
	var file *Word
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
		if strings.HasPrefix(v, "--") {
			switch {
			case v == "--quiet" || v == "--silent" || v == "--regexp-extended" || v == "--separate" ||
				v == "--unbuffered" || v == "--null-data" || v == "--posix" || v == "--sandbox" ||
				v == "--debug" || strings.HasPrefix(v, "--in-place") || strings.HasPrefix(v, "--line-length="):
				i++
			case strings.HasPrefix(v, "--expression="):
				codes = append(codes, Word{Known: true, Value: strings.TrimPrefix(v, "--expression=")})
				i++
			case v == "--expression" && i+1 < len(args):
				codes = append(codes, args[i+1])
				i += 2
			case strings.HasPrefix(v, "--file="):
				f := Word{Known: true, Value: strings.TrimPrefix(v, "--file=")}
				file = &f
				i++
			default:
				return nil, nil, unreadablef("%s option %s is not modelled", name, v)
			}
			continue
		}
		i++
		for k := 1; k < len(v); k++ {
			c := v[k]
			last := k == len(v)-1
			switch c {
			case 'n', 'E', 'r', 's', 'u', 'z':
			case 'i':
				k = len(v)
			case 'e', 'f', 'l':
				var arg Word
				if !last {
					arg = Word{Known: true, Value: v[k+1:]}
				} else {
					if i >= len(args) {
						return nil, nil, unreadablef("%s -%c without a value", name, c)
					}
					arg = args[i]
					i++
				}
				switch c {
				case 'e':
					codes = append(codes, arg)
				case 'f':
					a := arg
					file = &a
				}
				k = len(v)
			default:
				return nil, nil, unreadablef("%s option -%c is not modelled", name, c)
			}
		}
	}
	var inline *Inline
	if len(codes) > 0 {
		parts := make([]string, 0, len(codes))
		for _, c := range codes {
			text, err := knownValue(c, name+" script")
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, text)
		}
		inline = &Inline{Language: "sed", Source: Word{Known: true, Value: strings.Join(parts, "\n")}}
	}
	if inline == nil && file == nil {
		if i >= len(args) {
			return nil, nil, unreadablef("%s without a script", name)
		}
		if !args[i].Known {
			return nil, nil, unreadablef("%s script is not known (%s)", name, args[i].Reason)
		}
		inline = &Inline{Language: "sed", Source: args[i]}
	}
	return inline, file, nil
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
// after the options, not the word that follows -c. The operand is a script file unless -c or -s is set; without an
// operand, and with -s, the program comes from standard input (with -s the operands are positional parameters, not a script).
func (w *walker) shellCall(name string, args []Word, redirs []Redir, st *state, ctx Context) error {
	cmdMode, stdinMode := false, false
	i := 0
loop:
	for i < len(args) {
		v, err := knownValue(args[i], name+" option")
		if err != nil {
			return err
		}
		switch {
		case v == "--" || v == "-":
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
				switch c {
				case 'c':
					cmdMode = true
				case 's':
					stdinMode = true // +s is read like -s: the reader does not take a reading that lets a pipe through
				case 'o', 'O':
					i++ // -o and -O take the next word as their value
					if strings.ContainsAny(v[k+1:], "cs") {
						// bash goes on reading flags after -o (-oc posix is -o posix -c), zsh reads the rest of the word as the option
						// name: a -c or -s behind -o is read one way by one shell and the other way by the other, so it is refused.
						return unreadablef("%s option cluster %s has -c or -s behind -o", name, v)
					}
					k = len(v)
				default:
					if strings.IndexByte(shellFlagLetters, c) < 0 {
						return unreadablef("%s option -%c is not modelled", name, c)
					}
				}
			}
		default:
			break loop
		}
	}
	var operand *Word
	if i < len(args) {
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
	if operand != nil && !stdinMode {
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
	body, err := stdinProgram(redirs, ctx.Stdin, name)
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

// clusterInterp reads interpreter options. It returns the program words it found, the index of the first operand, for a module option (python -m) the module word (the operand index then points at the module's own arguments), and whether an option keeps the interpreter reading commands from standard input (-i).
// Node's --eval and --print take the program as the next word or after an =.
func clusterInterp(name string, args []Word, spec interpSpec) ([]Word, int, *Word, bool, error) {
	var codes []Word
	repl := false
	i := 0
	for i < len(args) {
		v, err := knownValue(args[i], name+" option")
		if err != nil {
			return nil, 0, nil, false, err
		}
		if v == "--" {
			i++
			break
		}
		if v == "--eval" || v == "--print" {
			if i+1 >= len(args) {
				return nil, 0, nil, false, unreadablef("%s %s without a program", name, v)
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
			return nil, 0, nil, false, unreadablef("%s option %s is not modelled", name, v)
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
					return nil, 0, nil, false, unreadablef("%s option -%c must stand alone", name, c)
				}
				if !last {
					codes = append(codes, Word{Known: true, Value: v[k+1:]})
				} else {
					if i >= len(args) {
						return nil, 0, nil, false, unreadablef("%s -%c without a program", name, c)
					}
					codes = append(codes, args[i])
					i++
				}
				k = len(v)
			case strings.IndexByte(spec.module, c) >= 0:
				var mod Word
				if !last {
					mod = Word{Known: true, Value: v[k+1:]}
				} else {
					if i >= len(args) {
						return nil, 0, nil, false, unreadablef("%s -%c without a module", name, c)
					}
					mod = args[i]
					i++
				}
				return codes, i, &mod, repl, nil
			case strings.IndexByte(spec.consume, c) >= 0:
				if last {
					if i >= len(args) {
						return nil, 0, nil, false, unreadablef("%s -%c without a value", name, c)
					}
					i++
				}
				k = len(v)
			case strings.IndexByte(spec.flags, c) >= 0:
				if strings.IndexByte(spec.repl, c) >= 0 {
					repl = true
				}
			default:
				return nil, 0, nil, false, unreadablef("%s option -%c is not modelled", name, c)
			}
		}
	}
	return codes, i, nil, repl, nil
}

// fdAliasPath reports a path that names a file descriptor of this process, so that reading it reads what the shell gave
// that descriptor: /dev/stdin, /dev/fd/N and /proc/<pid>/fd/N (and /proc/self/fd/N). The kernel resolves a path one component
// at a time, so the check does too: a dot segment and a doubled slash are nothing, a dot-dot segment steps back from the
// directory reached so far, and the process links /proc/<pid>/root and /proc/<pid>/cwd are replaced by their target the
// moment they are reached, before the next component (so /proc/self/root/../../dev/stdin is /dev/stdin, not /proc/dev/stdin).
// A relative path is placed in the directory the command runs in. In a directory the reader does not know, a relative path is
// the alias when some directory could make it one: its last component is stdin, or a descriptor number that stands alone or
// follows fd (dev/stdin, fd/0, proc/self/fd/0, 0); an ordinary script path (../tools/gen.py, build/fd/gen.py) is not.
func fdAliasPath(p string, dir Dir) bool {
	if p == "" {
		return false
	}
	if dir.Unset && !path.IsAbs(p) {
		return false // a reading with no directory: the readings that have one judge a relative path
	}
	w := aliasWalk{abs: path.IsAbs(p), dir: dir}
	if !w.abs && dir.Known {
		w.abs = true
		if w.feed(dir.Path) {
			return true
		}
	}
	if w.feed(p) {
		return true
	}
	if w.abs {
		return false
	}
	return relativeFdAlias(w.stack)
}

// aliasWalk is the directory fdAliasPath has reached: the components of an absolute path (abs), or the components of a path
// relative to a directory the reader does not know (leading ".." components are kept).
type aliasWalk struct {
	abs   bool
	stack []string
	dir   Dir
	depth int
}

// feed moves the walk along a path. It reports true as soon as the walk is at a descriptor alias, or at a link whose target
// the reader cannot say (the cwd of another process, or of this one when the directory is unknown).
func (w *aliasWalk) feed(p string) bool {
	w.depth++
	if w.depth > 8 {
		return true // directories that name process links to each other, deeper than the check follows
	}
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			if n := len(w.stack); n > 0 && w.stack[n-1] != ".." {
				w.stack = w.stack[:n-1]
			} else if !w.abs {
				w.stack = append(w.stack, "..")
			}
			continue
		}
		w.stack = append(w.stack, c)
		if w.abs && absFdAlias(w.stack) {
			return true
		}
		if pid, link := procLink(w.stack); link != "" {
			switch link {
			case "root":
				w.abs, w.stack = true, nil
			case "cwd":
				if (pid != "self" && pid != "thread-self") || !w.dir.Known {
					return true
				}
				w.abs, w.stack = true, nil
				if w.feed(w.dir.Path) {
					return true
				}
			}
		}
	}
	return false
}

// procLink names the process link a walk has just reached: /proc/<pid>/root, /proc/<pid>/cwd, or the same below
// /proc/<pid>/task/<tid>. It returns the pid component and the link name, or an empty link.
func procLink(s []string) (pid, link string) {
	for len(s) > 0 && s[0] == ".." {
		s = s[1:]
	}
	if len(s) == 0 || s[0] != "proc" {
		return "", ""
	}
	switch {
	case len(s) == 3 && (s[2] == "root" || s[2] == "cwd"):
		return s[1], s[2]
	case len(s) == 5 && s[2] == "task" && (s[4] == "root" || s[4] == "cwd"):
		return s[1], s[4]
	}
	return "", ""
}

// absFdAlias is whether an absolute path (as components) is, or is below, a descriptor alias.
func absFdAlias(s []string) bool {
	switch {
	case len(s) >= 2 && s[0] == "dev" && s[1] == "stdin":
		return true
	case len(s) >= 3 && s[0] == "dev" && s[1] == "fd":
		return true
	case len(s) >= 4 && s[0] == "proc" && s[2] == "fd":
		return true
	case len(s) >= 6 && s[0] == "proc" && s[2] == "task" && s[4] == "fd":
		return true
	}
	return false
}

// relativeFdAlias is whether a relative path in a directory the reader does not know could be a descriptor alias: the
// directory could be /, /dev, /dev/fd, /proc/self or /proc/self/fd (or below any directory, with leading dot-dot components).
func relativeFdAlias(s []string) bool {
	for len(s) > 0 && s[0] == ".." {
		s = s[1:]
	}
	n := len(s)
	if n == 0 {
		return false
	}
	if s[n-1] == "stdin" {
		return true
	}
	if isDigits(s[n-1]) && n == 1 {
		return true
	}
	for i := 0; i+1 < n; i++ {
		if s[i] == "fd" && isDigits(s[i+1]) {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// pythonModuleFlags are the options of python -m json.tool that take no value.
var pythonModuleFlags = []string{"--sort-keys", "--no-ensure-ascii", "--tab", "--compact", "--no-indent", "--json-lines", "-h", "--help"}

// checkPythonModule reads python -m MODULE. The module json.tool reads JSON from standard input and runs no program, so the
// issue's control (printf x | python3 -m json.tool) is read: the module and its options are all the text shows. Every other
// module may run what it reads (pdb, code, runpy, http.server, a module the working directory holds), and so may an operand of
// json.tool (it names a file the module writes); they are unreadable. The options are a closed list: a flag, or --indent with an
// integer. The module is the standard library's only when the working directory, the first entry of the module search path of
// python -m, holds no module named json (see pythonJSONShadow): a text that does not show the directory, or shows a json
// module there or writes one, runs code the reader cannot read.
func (w *walker) checkPythonModule(name string, module Word, rest []Word, dir Dir) error {
	mod, err := knownValue(module, name+" -m module")
	if err != nil {
		return err
	}
	if mod != "json.tool" {
		return unreadablef("%s -m %s runs a module the reader cannot read", name, mod)
	}
	if err := w.pythonJSONShadow(name, dir); err != nil {
		return err
	}
	for i := 0; i < len(rest); i++ {
		v, err := knownValue(rest[i], name+" -m json.tool option")
		if err != nil {
			return err
		}
		switch {
		case slices.Contains(pythonModuleFlags, v):
		case v == "--indent":
			if i+1 >= len(rest) || !rest[i+1].Known || !allDigits(rest[i+1].Value) {
				return unreadablef("%s -m json.tool --indent without an integer", name)
			}
			i++
		case strings.HasPrefix(v, "--indent=") && allDigits(strings.TrimPrefix(v, "--indent=")):
		default:
			return unreadablef("%s -m json.tool argument %q is outside the options the reader models", name, v)
		}
	}
	return nil
}

func allDigits(s string) bool {
	if s == "" || len(s) > 3 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// pythonJSONShadowNames are the files and package files of the working directory that python finds before the standard library
// json: a source or byte-code module json.py and json.pyc, and the __init__ of a json package.
var pythonJSONShadowNames = []string{"json.py", "json.pyc", "json/__init__.py", "json/__init__.pyc"}

// pythonJSONShadow proves that python -m json.tool runs the standard library: the directory is known, no earlier record of the
// text writes a module that would be found in it, and the directory holds none (a json package with an __init__, json.py,
// json.pyc, or a compiled json extension module). A directory the reader cannot list holds nothing it can prove; one that does
// not exist holds no module.
func (w *walker) pythonJSONShadow(name string, dir Dir) error {
	if dir.Unset {
		return nil // a reading with no directory: the readings that have one make this judgment
	}
	if !dir.Known || dir.Path == "" {
		return unreadablef("%s -m json.tool searches the working directory first and the reader does not know it", name)
	}
	for _, n := range pythonJSONShadowNames {
		if w.createdByText(n, dir) {
			return unreadablef("%s -m json.tool runs %s, which this text writes", name, n)
		}
	}
	entries, err := os.ReadDir(dir.Path)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return unreadablef("%s -m json.tool: the working directory %s cannot be listed: %v", name, dir.Path, err)
	}
	for _, e := range entries {
		n := e.Name()
		switch {
		case n == "json.py", n == "json.pyc", n == "json.pyw",
			strings.HasPrefix(n, "json.") && (strings.HasSuffix(n, ".so") || strings.HasSuffix(n, ".pyd")):
			return unreadablef("%s -m json.tool would run %s of the working directory, not the standard library", name, n)
		case n == "json":
			sub, err := os.ReadDir(filepath.Join(dir.Path, n))
			switch {
			case err == nil:
			case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
				continue
			default:
				return unreadablef("%s -m json.tool: the directory json cannot be listed: %v", name, err)
			}
			for _, f := range sub {
				if fn := f.Name(); strings.HasPrefix(fn, "__init__.") {
					return unreadablef("%s -m json.tool would run the json package of the working directory", name)
				}
			}
		}
	}
	return nil
}
