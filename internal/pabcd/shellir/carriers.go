package shellir

import "strings"

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
	code    string // letters whose argument is program text
	consume string // letters whose argument is not program text
	flags   string // letters with no argument
	attach  bool   // a code letter may carry its program in the same word
}

func interpreterSpec(lang string) interpSpec {
	switch lang {
	case "python":
		return interpSpec{code: "c", consume: "WX", flags: "BbdEhiIOPqRsSuvxV", attach: true}
	case "node":
		return interpSpec{code: "ep", consume: "r", flags: "ci", attach: true}
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
		return nil, nil, nil
	}
	text, err := stdinProgram(redirs, ctx.Stdin, name)
	if err != nil {
		return nil, nil, err
	}
	return &Inline{Language: lang, Source: Word{Known: true, Value: text}}, nil, nil
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
// after the options, not the word that follows -c. The operand is a script file unless -c is set; without an
// operand the program comes from standard input.
func (w *walker) shellCall(name string, args []Word, redirs []Redir, st *state, ctx Context) error {
	cmdMode := false
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
				if c == 'c' {
					cmdMode = true
					continue
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
	if operand != nil {
		return w.scriptFile(name, *operand, st, ctx)
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
