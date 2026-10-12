package shellir

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"mvdan.cc/sh/v3/syntax"
)

func quotedArgs(word *syntax.Word) bool {
	if len(word.Parts) != 1 {
		return false
	}
	quoted, ok := word.Parts[0].(*syntax.DblQuoted)
	if !ok || len(quoted.Parts) != 1 {
		return false
	}
	parameter, ok := quoted.Parts[0].(*syntax.ParamExp)
	return ok && parameter.Param != nil && parameter.Param.Value == "@" && parameter.Exp == nil && parameter.Slice == nil && parameter.Repl == nil && parameter.Index == nil && parameter.NestedParam == nil && !parameter.Length && !parameter.Width && !parameter.Excl && parameter.Names == 0 && len(parameter.Modifiers) == 0 && parameter.Flags == nil
}

func pathAssignmentPrefix(word *syntax.Word) string {
	parts := word.Parts
	if len(parts) == 1 {
		if quoted, ok := parts[0].(*syntax.DblQuoted); ok {
			parts = quoted.Parts
		}
	}
	if len(parts) > 0 {
		if literal, ok := parts[0].(*syntax.Lit); ok && strings.HasPrefix(literal.Value, "PATH=") {
			return "PATH"
		}
	}
	return ""
}

// declaredPath gives a declaration builtin that the walk reaches as a command (an assignment in front of it)
// the PATH a declaration clause gives: `X=1 export PATH=/dir; prog` runs prog from /dir (CRW-1283, found by the differential fuzz).
func declaredPath(name string, args []Word, st *state) {
	switch name {
	case "export", "declare", "typeset", "readonly", "local":
	default:
		return
	}
	for _, arg := range args {
		switch {
		case !arg.Known && arg.AssignmentName == "PATH":
			st.setVar("PATH", Word{Reason: arg.Reason}, false)
		case arg.Known:
			if variable, value, ok := splitAssignment(arg.Value); ok && variable == "PATH" {
				st.setVar("PATH", Word{Known: true, Value: value}, false)
			} else if variable, value, ok := splitAssignment(strings.Replace(arg.Value, "PATH+=", "PATH_APPEND=", 1)); ok && variable == "PATH_APPEND" {
				st.setVar("PATH", Word{Known: true, Value: value}, true)
			}
		default:
			continue
		}
		st.pathTouched = true
	}
}

func samePath(left, right *Word) bool {
	return left == right || left != nil && right != nil && *left == *right
}

// pathIndependent is whether every shell the reader models (sh and dash, bash, zsh, ksh) runs a name itself without searching
// the PATH: a function the text defines, a POSIX special builtin, and the builtins POSIX has the shell find before the PATH
// (cd, command, getopts, read). A name that is a builtin or keyword in some shells only (printf, echo, test, time, let, declare,
// local, pushd, mapfile, chdir, source, builtin and the rest) is a program the PATH finds in another (dash has no let, bash no
// chdir, dash's time is a program), so it is searched on the assigned PATH (CRW-1283, Sion decision 5).
func pathIndependent(name string, st *state) bool {
	if _, ok := st.funcs[name]; ok {
		return true
	}
	return posixShellOwn(name)
}

// posixShellOwn names the POSIX special builtins and the builtins every modelled shell runs before a search of the PATH.
func posixShellOwn(name string) bool {
	switch name {
	case ":", ".", "break", "continue", "eval", "exec", "exit", "export", "readonly", "return", "set", "shift", "times", "trap",
		"unset", "cd", "command", "getopts", "read":
		return true
	}
	return false
}

// shellOwnSomewhere names the builtins and keywords that some modelled shells run themselves and others search on the PATH: a
// name of these that no directory of the PATH holds is the shell's own (or not found), and is read by its name.
func shellOwnSomewhere(name string) bool {
	switch name {
	case "echo", "printf", "true", "false", "test", "[", "chdir", "pushd", "popd", "dirs", "source", "mapfile", "readarray",
		"declare", "typeset", "local", "let", "builtin", "time", "enable", "disable", "shopt", "setopt", "unsetopt", "print",
		"pwd", "type", "hash", "alias", "unalias", "umask", "wait", "kill", "jobs", "fg", "bg", "ulimit", "fc", "nameref",
		"caller", "compgen", "complete", "help", "history", "logout", "suspend", "disown", "unhash", "unfunction", "whence",
		"where", "which", "autoload", "zmodload", "functions", "noglob", "emulate", "integer", "float", "zparseopts":
		return true
	}
	return false
}

// keywordOnPath answers for a clause whose word is a builtin or keyword in bash and zsh and a program the PATH finds in sh (dash):
// a declaration, let, time, [[, coproc, select or function. Behind an assigned PATH, a file of that name is refused unless the PATH
// the shell starts with runs the same file, since the clause is read as the builtin (CRW-1283, Sion decision 5). A name no
// directory holds is read as before. The directory the file stands in decides nothing.
func (w *walker) keywordOnPath(name string, st *state) error {
	if st.searchPath == nil || posixShellOwn(name) && !w.builtinsChanged {
		return nil
	}
	if _, ok := st.funcs[name]; ok && !w.builtinsChanged {
		return nil
	}
	found, err := resolveProgram(Word{Known: true, Value: name}, st)
	switch {
	case err == errPathNotFound:
		return nil
	case err != nil:
		return err
	case !strings.Contains(found.Value, "/") || startingPathRuns(name, found.Value):
		return nil
	}
	return unreadablef("%s is a builtin in some shells and %s on the assigned PATH in others; run it with a PATH that does not hold it", name, found.Value)
}

func resolveProgram(program Word, st *state) (Word, error) {
	if st.dir.Unset {
		return program, nil
	}
	if !st.searchPath.Known {
		return Word{}, unreadablef("PATH is not known; the external program may run protected operations")
	}
	for _, directory := range strings.Split(st.searchPath.Value, ":") {
		if !filepath.IsAbs(directory) {
			if !st.dir.Known {
				return Word{}, unreadablef("PATH has a relative directory and the working directory is unknown")
			}
			directory = strings.TrimSuffix(st.dir.Path, "/") + "/" + directory
		}
		candidate := strings.TrimSuffix(directory, "/") + "/" + program.Value
		info, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Word{}, unreadablef("PATH program cannot be inspected")
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return Word{Known: true, Value: candidate}, nil
		}
	}
	return Word{}, errPathNotFound
}

// errPathNotFound is resolveProgram's answer for a name no directory of a known PATH holds.
var errPathNotFound = unreadablef("PATH does not resolve the external program")

// prefixPath gives a PATH assignment in front of a command its effect: the command (and what it carries) finds programs on that
// PATH, and the returned function puts the PATH of the shell back once the command is read. Everything else the command does to
// the shell (cd, read, printf -v) stays, as it does when the assignment lasts for the command only. A POSIX special builtin keeps
// such an assignment after it in some shells and not in others, and a function call does in bash's POSIX mode, so these are
// refused rather than read one way (CRW-1283).
func (s *state) prefixPath(name string, words []Word, assigns []Assign, launched bool) (func(), error) {
	assigned := false
	for _, a := range assigns {
		assigned = assigned || a.Name == "PATH"
	}
	if !assigned {
		return nil, nil
	}
	_, function := s.funcs[name]
	if !launched && (function || specialBuiltin(name) && !(name == "exec" && execRunsProgram(words[1:]))) {
		return nil, unreadablef("an assignment to PATH in front of %s may outlast it in some shells; assign PATH in a statement of its own", name)
	}
	saved := s.searchPath
	savedValue, hadValue := s.vars["PATH"]
	for _, a := range assigns {
		if a.Name == "PATH" {
			s.setVar(a.Name, a.Value, a.Append)
		}
	}
	temporary := s.searchPath
	return func() {
		if s.searchPath != temporary {
			// The command itself changed what the walk knows of PATH (read, declare): the PATH after it is not known.
			s.searchPath = &Word{Reason: "PATH may have changed"}
			delete(s.vars, "PATH")
			return
		}
		s.searchPath = saved
		if hadValue {
			s.vars["PATH"] = savedValue
		} else {
			delete(s.vars, "PATH")
		}
	}, nil
}

// execRunsProgram is whether exec's operands name a program: exec with only redirections changes the shell itself.
func execRunsProgram(args []Word) bool {
	for _, a := range args {
		if !a.Known || !strings.HasPrefix(a.Value, "-") {
			return true
		}
	}
	return false
}

// specialBuiltin names the POSIX special builtins (and source, bash's spelling of .): an assignment in front of one outlasts it in
// a POSIX shell.
func specialBuiltin(name string) bool {
	switch name {
	case "break", ":", ".", "source", "continue", "eval", "exec", "exit", "export", "readonly", "return", "set", "shift", "times",
		"trap", "unset":
		return true
	}
	return false
}

// positionalChange is whether a builtin may change the positional parameters that a later "$@" expands to: set with an operand
// or with -- or -, shift, and the builtins that run text of their own (eval, source, trap) or redefine words (alias).
func positionalChange(name string, args []Word) bool {
	switch name {
	case "shift", "eval", "source", ".", "trap", "alias":
		return true
	case "set":
		return !setOptionsOnly(args)
	}
	return false
}

// setOptionsOnly is whether set's words are options alone (set -euo pipefail, set +x): each word a cluster of option letters, an
// o last in a cluster taking the next word as the name of an option.
func setOptionsOnly(args []Word) bool {
	for i := 0; i < len(args); i++ {
		if !args[i].Known {
			return false
		}
		v := args[i].Value
		if len(v) < 2 || v[0] != '-' && v[0] != '+' {
			return false
		}
		for k := 1; k < len(v); k++ {
			c := v[k]
			if c == 'o' {
				if k != len(v)-1 {
					return false
				}
				if i+1 < len(args) {
					i++
					if !args[i].Known || !optionName(args[i].Value) {
						return false
					}
				}
				continue
			}
			if !strings.ContainsRune("abefhkmnptuvxBCEHPT", rune(c)) {
				return false
			}
		}
	}
	return true
}

// positionalAssignment is whether a program word is an assignment to a positional parameter in zsh (1=x, 2+=x, argv=x, *=x): bash
// runs it as a program of that name, zsh changes what a later "$@" expands to (CRW-1283).
func positionalAssignment(word string) bool {
	i := strings.IndexByte(word, '=')
	if i <= 0 {
		return false
	}
	name := strings.TrimSuffix(word[:i], "+")
	if j := strings.IndexByte(name, '['); j >= 0 {
		name = name[:j]
	}
	return positionalName(name)
}

// positionalName is whether a variable name is a positional parameter or zsh's array of them: argv, *, @, # or digits.
func positionalName(name string) bool {
	switch name {
	case "argv", "*", "@", "#":
		return true
	}
	return name != "" && strings.Trim(name, "0123456789") == ""
}

func optionName(v string) bool {
	if v == "" {
		return false
	}
	for _, c := range v {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// pathProgramIdentified admits a program an assigned PATH finds, in any directory (a directory name such as /usr/local/bin, and a
// file header, establish nothing about what a program is). A text file (a script) is read
// by the consumers like a script run by its path, unless its name is an interpreter's: the reader then reads the interpreter's
// inline program or module by the name, not the script. A binary is read by its name only, which says nothing of a copy or a link
// named otherwise. Either is admitted only when the PATH the shell started with (the environment of this process) runs the same
// file for the name: the assignment then does not change which program runs. Any other is refused (CRW-1283, Sion decision 5).
func pathProgramIdentified(name, program string) error {
	binary, err := programIsBinary(program)
	if err != nil {
		return err
	}
	byName := isInterpreter(name) || isPythonName(name) || isOpaqueInterpreter(name)
	if !binary && !byName {
		return nil
	}
	if startingPathRuns(name, program) {
		return nil
	}
	if !binary {
		return unreadablef("%s is a script that the assigned PATH finds under the interpreter name %s and the PATH the shell starts with does not run; the reader would read the interpreter, not the script", program, name)
	}
	return unreadablef("%s is a binary that the assigned PATH finds and the PATH the shell starts with does not run; run it by its absolute path", program)
}

// programIsBinary is whether an executable file is a binary: a file whose head has no #! line and a NUL, as the consumers read it.
func programIsBinary(program string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(program)
	if err != nil {
		return false, unreadablef("PATH program cannot be inspected")
	}
	descriptor, err := syscall.Open(resolved, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, unreadablef("PATH program cannot be inspected")
	}
	file := os.NewFile(uintptr(descriptor), resolved)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, unreadablef("PATH program is not a regular file")
	}
	head := make([]byte, 4096)
	count, err := io.ReadFull(file, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, unreadablef("PATH program cannot be read")
	}
	return !bytes.HasPrefix(head[:count], []byte("#!")) && bytes.IndexByte(head[:count], 0) >= 0, nil
}

// startingPathRuns is whether the PATH the shell starts with runs the same file for a name as the file an assigned PATH found.
func startingPathRuns(name, program string) bool {
	ambient, ok := ambientProgram(name)
	if !ok {
		return false
	}
	left, leftErr := os.Stat(program)
	right, rightErr := os.Stat(ambient)
	return leftErr == nil && rightErr == nil && os.SameFile(left, right)
}

// EnvShebang judges the interpreter a #! line launches through env for a program run with an assigned PATH (path is that PATH, nil
// when none is assigned): env finds the interpreter on the PATH the program runs with, so the file the assigned PATH selects is the
// interpreter, and what the reader makes of the script's body says nothing about it. The interpreter is admitted only when the
// PATH the shell starts with runs the same file for its name; an env option that changes the search or the environment (-i, -u,
// -P, --, a NAME=value) cannot be followed and is refused. An interpreter named by a path, and a line that does not go through
// env, do not depend on the PATH (CRW-1283, the evaluation of deb45bd1, d2).
func EnvShebang(line string, path *Word, dir Dir) error {
	words := strings.Fields(strings.TrimPrefix(line, "#!"))
	if path == nil || len(words) == 0 || filepath.Base(words[0]) != "env" {
		return nil
	}
	args := words[1:]
	if len(args) > 0 && args[0] == "-S" {
		args = args[1:]
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || strings.Contains(args[0], "=") {
		return unreadablef("the #! line launches its interpreter through env with an option the reader cannot follow under an assigned PATH")
	}
	name := args[0]
	if strings.Contains(name, "/") {
		return nil
	}
	found, err := resolveProgram(Word{Known: true, Value: name}, &state{dir: dir, searchPath: path})
	switch {
	case err != nil:
		return err
	case !strings.Contains(found.Value, "/"):
		return unreadablef("the interpreter %s of the #! line cannot be found on the assigned PATH", name)
	case !startingPathRuns(name, found.Value):
		return unreadablef("the #! line launches %s through env, the assigned PATH finds %s, and the PATH the shell starts with does not run that file", name, found.Value)
	}
	return nil
}

// ambientPath is the PATH the shell that runs the command starts with: the environment of this process (a hook runs in the
// environment of the session whose command it judges). A variable, so a test can give it.
var ambientPath = func() (string, bool) { return os.LookupEnv("PATH") }

// ambientProgram is the file the PATH the shell starts with runs for a name. A relative or empty directory before the match names
// a place the reader does not fix, so it answers none.
func ambientProgram(name string) (string, bool) {
	value, ok := ambientPath()
	if !ok {
		return "", false
	}
	for _, directory := range strings.Split(value, ":") {
		if !filepath.IsAbs(directory) {
			return "", false
		}
		candidate := strings.TrimSuffix(directory, "/") + "/" + name
		info, err := os.Stat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}
