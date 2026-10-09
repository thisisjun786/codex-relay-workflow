package hook

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// shellIRSedScriptDests returns the files a sed command's script writes: the file of a w or W command and of the w flag
// of s, read from the script text the command gives (-e, --expression, any unambiguous abbreviation of it, or the first
// operand when no -e or -f is given). The options are read as getopt_long reads them (shellir.ParseSedArgs). A script file
// (-f) is not read here: its text is not in the command, and the walker records it as a script file the gates cannot read. A
// word or script this reader cannot read names the unknown destination.
func shellIRSedScriptDests(args []shellir.Word) []string {
	unknown := []string{shellIRUnknownDest}
	for _, a := range args {
		if shellIRPlain(a) == shellIRUnknownDest {
			return unknown
		}
	}
	pa, err := shellir.ParseSedArgs("sed", args)
	if err != nil {
		return unknown
	}
	scripts := pa.Scripts
	if len(scripts) == 0 && len(pa.Files) == 0 && len(pa.Operands) > 0 {
		scripts = pa.Operands[:1]
	}
	var out []string
	for _, w := range scripts {
		for _, d := range shellSedWriteDests(shellIRPlain(w)) {
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
		case 'e':
			return unknown // e runs a shell command the script names
		case 'r', 'R', 'a', 'i', 'c':
			toEOL()
		case ':', 'b', 't', 'T':
			// A label ends at a blank, a semicolon or a brace: GNU sed goes on with the commands after it (b end;w f). A
			// reading that ended it later would skip a w the script runs, so the earliest end is taken.
			for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
				i++
			}
			for i < len(rs) && !strings.ContainsRune(" \t\n;}", rs[i]) {
				i++
			}
		case 'l', 'L', 'q', 'Q':
			// An optional number (a line width or an exit code), then the next command: sed opens every w file when it
			// reads the script, so a w after q is a write too.
			for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
				i++
			}
			for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
				i++
			}
		case '{', '}':
			// a block opened or closed after an address: the commands inside are read in turn
		case '=', 'd', 'D', 'g', 'G', 'h', 'H', 'n', 'N', 'p', 'P', 'x', 'z', 'F':
		case 's':
			if i >= len(rs) {
				return unknown
			}
			d := rs[i]
			i++
			// s/pattern/replacement/ and y/source/dest/ each end two parts at the delimiter d.
			if !delimited(d) {
				return unknown
			}
			if !delimited(d) {
				return unknown
			}
			for i < len(rs) && strings.ContainsRune("gpiImM0123456789", rs[i]) {
				i++
			}
			if i < len(rs) && rs[i] == 'e' {
				return unknown // the e flag runs the replacement as a shell command
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
			// s/pattern/replacement/ and y/source/dest/ each end two parts at the delimiter d.
			if !delimited(d) {
				return unknown
			}
			if !delimited(d) {
				return unknown
			}
		default:
			return unknown
		}
	}
	return out
}
