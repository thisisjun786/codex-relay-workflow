package shellir

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// pipeProducer returns the bytes a pipe's left side writes when it is a literal printf or echo the text shows. Any other
// producer, a word the reader cannot evaluate without running it, or a conversion the reader does not model gives no bytes.
func pipeProducer(s *syntax.Stmt) (string, bool) {
	if s == nil || s.Negated || s.Background || s.Coprocess || len(s.Redirs) > 0 {
		return "", false
	}
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) == 0 {
		return "", false
	}
	words := make([]string, 0, len(call.Args))
	for _, x := range call.Args {
		v, ok := literalWord(x)
		if !ok {
			return "", false
		}
		words = append(words, v)
	}
	switch words[0] {
	case "printf":
		return printfText(words[1:])
	case "echo":
		return echoText(words[1:])
	}
	return "", false
}

// simpleCallStmt reports a statement that is one simple command, the right side of a pipe that the pipe's bytes can
// feed directly: a compound command or a negated one is not read through a pipe.
func simpleCallStmt(s *syntax.Stmt) bool {
	if s == nil || s.Negated || s.Background || s.Coprocess {
		return false
	}
	_, ok := s.Cmd.(*syntax.CallExpr)
	return ok
}

// literalWord returns the value of a word made only of literal text, single-quoted text and double-quoted literal text. A
// word with a glob, brace, tilde or backslash in its literal part, or any expansion, gives no value.
func literalWord(x *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, p := range x.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "\\*?[]{}~") {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok || strings.Contains(l.Value, "\\") {
					return "", false
				}
				b.WriteString(l.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// printfText evaluates a printf whose only argument is the format, with the escapes and the %% conversion it models.
// Any other argument, an option, or a conversion gives no bytes.
func printfText(args []string) (string, bool) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return "", false
	}
	f := args[0]
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		switch c := f[i]; c {
		case '\\':
			if i+1 >= len(f) {
				return "", false
			}
			i++
			switch f[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case '\'':
				b.WriteByte('\'')
			default:
				return "", false
			}
		case '%':
			if i+1 >= len(f) || f[i+1] != '%' {
				return "", false
			}
			i++
			b.WriteByte('%')
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// echoText evaluates an echo of plain words: an optional leading -n, and no backslash, which shells disagree on. A word
// after the options is text even when it starts with a dash.
func echoText(args []string) (string, bool) {
	newline := true
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] != "-n" {
			return "", false
		}
		newline = false
		args = args[1:]
	}
	for _, a := range args {
		if strings.Contains(a, "\\") {
			return "", false
		}
	}
	out := strings.Join(args, " ")
	if newline {
		out += "\n"
	}
	return out, true
}

// hasStdinRedirect reports a redirection of the program's own standard input (or a copy onto it): the program then reads
// that file, not the pipe, and zsh with MULTIOS reads both, so the pipe is not the program.
func hasStdinRedirect(redirs []Redir) bool {
	for _, r := range redirs {
		if r.Fd == "" || r.Fd == "0" {
			return true
		}
	}
	return false
}

// fdAliasNumber returns the descriptor a path names when it names one of this process's descriptors: /dev/stdin (0),
// /dev/fd/N and /proc/self/fd/N. Another process's descriptor is not the one the text sets.
func fdAliasNumber(p string) (string, bool) {
	switch {
	case p == "/dev/stdin":
		return "0", true
	case strings.HasPrefix(p, "/dev/fd/"):
		n := p[len("/dev/fd/"):]
		return n, isDigits(n)
	case strings.HasPrefix(p, "/proc/"):
		parts := strings.Split(p[len("/proc/"):], "/")
		if len(parts) == 3 && (parts[0] == "self" || parts[0] == "thread-self") && parts[1] == "fd" && isDigits(parts[2]) {
			return parts[2], true
		}
	}
	return "", false
}

// aliasProgram returns the program text that a descriptor alias runs: standard input's text when the alias is 0, otherwise the
// here-document or here-string the text sets on that descriptor.
func aliasProgram(path string, redirs []Redir, ctx Context, name string) (string, error) {
	n, ok := fdAliasNumber(path)
	if !ok {
		return "", unreadablef("%s reads its program from %s, a descriptor alias the reader does not model", name, path)
	}
	if n == "0" {
		return stdinProgram(redirs, ctx.Stdin, name, ctx)
	}
	return fdBody(redirs, n, len(redirs), 0)
}

// fdBody returns the body of the here-document or here-string that the first limit redirections set on descriptor fd. A
// descriptor copied from another (4<&3) takes the body of that one, when it was set before the copy.
func fdBody(redirs []Redir, fd string, limit, depth int) (string, error) {
	if depth > 4 {
		return "", unreadablef("descriptor copies nest deeper than four")
	}
	idx, count := -1, 0
	for i := 0; i < limit; i++ {
		if fdOf(redirs[i]) == fd {
			idx = i
			count++
		}
	}
	if count != 1 {
		return "", unreadablef("descriptor %s is set %d times by the text", fd, count)
	}
	r := redirs[idx]
	switch r.Op {
	case "<<", "<<-", "<<<":
		if !r.Target.Known {
			return "", unreadablef("descriptor %s holds a here-document that is not known (%s)", fd, r.Target.Reason)
		}
		return r.Target.Value, nil
	case "<&":
		if r.Target.Known && isDigits(r.Target.Value) {
			return fdBody(redirs, r.Target.Value, idx, depth+1)
		}
	}
	return "", unreadablef("descriptor %s is not a here-document or a copy of one", fd)
}

// fdOf names the descriptor a redirection sets: standard input when it has no number.
func fdOf(r Redir) string {
	if r.Fd == "" {
		return "0"
	}
	return r.Fd
}
