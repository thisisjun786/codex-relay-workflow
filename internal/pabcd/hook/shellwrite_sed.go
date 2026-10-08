package hook

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// shellIRSedScriptDests returns the files a sed command's script writes: the file of a w or W command and of the w flag
// of s, read from the script text the command gives (-e, --expression, or the first operand when no -e or -f is
// given). A script file (-f) is not read here: its text is not in the command. A script this reader cannot read names
// the unknown destination.
func shellIRSedScriptDests(args []shellir.Word) []string {
	var scripts, operands []string
	haveScript := false
	unknown := []string{shellIRUnknownDest}
	for i := 0; i < len(args); i++ {
		v := shellIRPlain(args[i])
		if v == shellIRUnknownDest {
			return unknown
		}
		switch {
		case v == "--":
			for _, rest := range args[i+1:] {
				operands = append(operands, shellIRPlain(rest))
			}
			i = len(args)
		case v == "-e" || v == "--expression" || v == "-f" || v == "--file" || v == "-l" || v == "--line-length":
			if i+1 >= len(args) {
				return unknown
			}
			i++
			val := shellIRPlain(args[i])
			if val == shellIRUnknownDest {
				return unknown
			}
			if v == "-e" || v == "--expression" {
				haveScript = true
				scripts = append(scripts, val)
			} else if v == "-f" || v == "--file" {
				haveScript = true
			}
		case strings.HasPrefix(v, "--expression="):
			haveScript = true
			scripts = append(scripts, strings.TrimPrefix(v, "--expression="))
		case strings.HasPrefix(v, "--file="):
			haveScript = true
		case strings.HasPrefix(v, "--"):
		case strings.HasPrefix(v, "-") && len(v) > 1:
			for j := 1; j < len(v); j++ {
				c := v[j]
				if c == 'e' || c == 'f' || c == 'l' {
					rest := v[j+1:]
					if rest == "" {
						if i+1 >= len(args) {
							return unknown
						}
						i++
						rest = shellIRPlain(args[i])
						if rest == shellIRUnknownDest {
							return unknown
						}
					}
					if c == 'e' {
						haveScript = true
						scripts = append(scripts, rest)
					} else if c == 'f' {
						haveScript = true
					}
					break
				}
				if c == 'i' {
					break // -i[SUFFIX] takes the rest of the word as its suffix
				}
			}
		default:
			operands = append(operands, v)
		}
	}
	if !haveScript {
		if len(operands) == 0 {
			return nil
		}
		scripts = append(scripts, operands[0])
	}
	var out []string
	for _, s := range scripts {
		for _, d := range shellSedWriteDests(s) {
			if !slicesContainsString(out, d) {
				out = append(out, d)
			}
		}
	}
	return out
}

func slicesContainsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// shellSedWriteDests reads one sed script for its writes. A w or W command names its file to the end of the line; an s
// command names the file of its w flag the same way. Addresses, labels, the text of a, i and c, and the file of r and R
// name no write. A command this reader does not model is unknown, and so is a script that ends inside a command.
func shellSedWriteDests(script string) []string {
	rs := []rune(script)
	unknown := []string{shellIRUnknownDest}
	var out []string
	i := 0
	toEOL := func() string {
		start := i
		for i < len(rs) && rs[i] != '\n' {
			i++
		}
		return string(rs[start:i])
	}
	// delimited reads up to the unescaped delimiter d and returns false when the script ends first.
	delimited := func(d rune) bool {
		for i < len(rs) {
			if rs[i] == '\\' {
				i += 2
				continue
			}
			if rs[i] == d {
				i++
				return true
			}
			i++
		}
		return false
	}
	for i < len(rs) {
		c := rs[i]
		if c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '}' || c == '{' {
			i++
			continue
		}
		if c == '#' {
			toEOL()
			continue
		}
		// addresses: a number, $, a /regex/ or \cregexc, first~step, addr,+N, then a !
		for i < len(rs) {
			switch {
			case rs[i] >= '0' && rs[i] <= '9', rs[i] == '$', rs[i] == '+', rs[i] == '~', rs[i] == ',', rs[i] == ' ':
				i++
				continue
			case rs[i] == '/' || rs[i] == '\\':
				d := rs[i]
				if d == '\\' {
					i++
					if i >= len(rs) {
						return unknown
					}
					d = rs[i]
				}
				i++
				if !delimited(d) {
					return unknown
				}
				for i < len(rs) && (rs[i] == 'I' || rs[i] == 'M') {
					i++
				}
				continue
			}
			break
		}
		for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t' || rs[i] == '!') {
			i++
		}
		if i >= len(rs) {
			break
		}
		c = rs[i]
		i++
		switch c {
		case 'w', 'W':
			name := strings.TrimSpace(toEOL())
			if name == "" {
				return unknown
			}
			out = append(out, name)
		case 'r', 'R', 'a', 'i', 'c', 'e', ':', 'b', 't', 'T', 'l', 'L', 'q', 'Q':
			toEOL()
		case '=', 'd', 'D', 'g', 'G', 'h', 'H', 'n', 'N', 'p', 'P', 'x', 'z', 'F':
		case 's':
			if i >= len(rs) {
				return unknown
			}
			d := rs[i]
			i++
			if !delimited(d) || !delimited(d) {
				return unknown
			}
			for i < len(rs) && strings.ContainsRune("gpiImMe0123456789", rs[i]) {
				i++
			}
			if i < len(rs) && rs[i] == 'w' {
				i++
				name := strings.TrimSpace(toEOL())
				if name == "" {
					return unknown
				}
				out = append(out, name)
			}
		case 'y':
			if i >= len(rs) {
				return unknown
			}
			d := rs[i]
			i++
			if !delimited(d) || !delimited(d) {
				return unknown
			}
		default:
			return unknown
		}
	}
	return out
}
