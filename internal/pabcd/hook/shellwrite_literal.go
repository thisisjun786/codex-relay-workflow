package hook

import "strings"

// These helpers compensate proven literal write omissions without changing the
// recorded oracle lexer used by the later verb port. They perform no expansion.
func literalRedirectDestinations(command string) []string {
	s := stripLiteralHeredocs(command)
	dests := []string{}
	for i := 0; i < len(s); {
		switch s[i] {
		case '\'', '"':
			_, i, _ = literalShellWord(s, i)
			continue
		case '\\':
			i = min(i+2, len(s))
			continue
		case '#':
			// Only a word-boundary # begins a shell comment.
			if i == 0 || strings.ContainsRune(" \t\r\n;|&()", rune(s[i-1])) {
				if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
					i += end + 1
				} else {
					i = len(s)
				}
				continue
			}
		case '<':
			if strings.HasPrefix(s[i:], "<<<") {
				_, i, _ = literalShellWord(s, i+3)
				continue
			}
			if strings.HasPrefix(s[i:], "<<") {
				at := i + 2
				if at < len(s) && s[at] == '-' {
					at++
				}
				_, i, _ = literalShellWord(s, at)
				continue
			}
		}
		readwrite := strings.HasPrefix(s[i:], "<>")
		if s[i] != '>' && !readwrite {
			i++
			continue
		}
		at := i + 1
		if readwrite || at < len(s) && (s[at] == '>' || s[at] == '|') {
			at++
		}
		dup := at < len(s) && s[at] == '&'
		if dup {
			at++
		}
		word, next, literal := literalShellWord(s, at)
		i = max(i+1, next)
		if !literal || word == "" || word == "/dev/null" || dup && shellDescriptor(word) {
			continue
		}
		dests = append(dests, word)
	}
	return dests
}

func shellDescriptor(word string) bool {
	if word == "-" {
		return true
	}
	for _, ch := range strings.TrimSuffix(word, "-") {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// POSIX token separation uses ASCII blanks, not JavaScript's Unicode whitespace.
// Quote fragments concatenate; unquoted escapes remove their backslash and
// double-quoted escapes remove it only for $, backtick, quote, backslash and LF.
func literalShellWord(s string, i int) (string, int, bool) {
	for i < len(s) && strings.ContainsRune(" \t\r\n", rune(s[i])) {
		i++
	}
	var out strings.Builder
	literal := true
	var quote byte
	for i < len(s) {
		ch := s[i]
		if quote == 0 && strings.ContainsRune(" \t\r\n;|&<>()", rune(ch)) {
			break
		}
		if ch == '\'' || ch == '"' {
			if quote == 0 {
				quote = ch
				i++
				continue
			}
			if quote == ch {
				quote = 0
				i++
				continue
			}
		}
		if ch == '\\' && quote != '\'' {
			if i+1 >= len(s) {
				return out.String(), i + 1, false
			}
			next := s[i+1]
			if quote == 0 || strings.ContainsRune("$`\"\\\n", rune(next)) {
				if next != '\n' {
					out.WriteByte(next)
				}
				i += 2
				continue
			}
		}
		if quote != '\'' && (ch == '$' || ch == '`') {
			literal = false
		}
		out.WriteByte(ch)
		i++
	}
	return out.String(), i, literal && quote == 0
}

type literalHeredoc struct {
	delimiter string
	tabs      bool
}

// Unlike the parity helper, this recognizes the complete quoted/escaped word,
// <<- tab stripping and multiple queued bodies. It always reads the raw command.
func stripLiteralHeredocs(s string) string {
	var out strings.Builder
	var pending []literalHeredoc
	for i := 0; i < len(s); {
		start := i
		switch s[i] {
		case '\'', '"':
			_, i, _ = literalShellWord(s, i)
			out.WriteString(s[start:i])
			continue
		case '\\':
			i = min(i+2, len(s))
			out.WriteString(s[start:i])
			continue
		case '#':
			if i == 0 || strings.ContainsRune(" \t\r\n;|&()", rune(s[i-1])) {
				if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
					i += end
				} else {
					i = len(s)
				}
				out.WriteString(s[start:i])
				continue
			}
		case '<':
			if strings.HasPrefix(s[i:], "<<<") {
				_, i, _ = literalShellWord(s, i+3)
				out.WriteString(s[start:i])
				continue
			}
			if strings.HasPrefix(s[i:], "<<") {
				at := i + 2
				tabs := at < len(s) && s[at] == '-'
				if tabs {
					at++
				}
				word, next, ok := literalShellWord(s, at)
				if ok && word != "" {
					pending = append(pending, literalHeredoc{word, tabs})
				}
				i = max(i+2, next)
				out.WriteString(s[start:i])
				continue
			}
		case '\n':
			out.WriteByte('\n')
			i++
			for _, h := range pending {
				i = literalHeredocEnd(s, i, h)
			}
			pending = nil
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func literalHeredocEnd(s string, i int, h literalHeredoc) int {
	for i < len(s) {
		end := strings.IndexByte(s[i:], '\n')
		nl := end >= 0
		if nl {
			end += i
		} else {
			end = len(s)
		}
		line := s[i:end]
		if h.tabs {
			line = strings.TrimLeft(line, "\t")
		}
		i = end
		if nl {
			i++
		}
		if line == h.delimiter {
			return i
		}
	}
	return i
}
