package hook

import "strings"

// These helpers compensate proven literal write omissions without changing the
// recorded oracle lexer used by the later verb port. They perform no expansion.
func literalRedirectDestinations(command string) []string {
	s := stripLiteralHeredocs(command)
	dests := []string{}
	wordStart := true
	for i := 0; i < len(s); {
		switch s[i] {
		case '\'', '"':
			_, i, _ = literalShellWord(s, i)
			wordStart = false
			continue
		case '\\':
			i = min(i+2, len(s))
			wordStart = false
			continue
		case '#':
			// Only a word-boundary # begins a shell comment.
			if wordStart {
				if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
					i += end + 1
					wordStart = true
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
			wordStart = strings.ContainsRune(" \t\n;|&()<>", rune(s[i]))
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
		wordStart = false
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
	return literalWord(s, i, false)
}

// Delimiters remove quotes but perform no expansion, including dollar words.
func literalWord(s string, i int, delimiter bool) (string, int, bool) {
	for i < len(s) && strings.ContainsRune(" \t\n", rune(s[i])) {
		i++
	}
	start := i
	var out strings.Builder
	literal := true
	var quote byte
	for i < len(s) {
		ch := s[i]
		if quote == 0 && strings.ContainsRune(" \t\n;|&<>()", rune(ch)) {
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
		if !delimiter && quote != '\'' && (ch == '$' || ch == '`') {
			literal = false
		}
		out.WriteByte(ch)
		i++
	}
	return out.String(), i, literal && quote == 0 && i > start
}

type literalHeredoc struct {
	delimiter    string
	tabs, quoted bool
}

// Read one complete shell header, joining continuations before lexing operators.
// Heredoc bodies are consumed separately, so their quotes cannot change header
// parsing. Escaped separators remain part of the word before a following #.
func literalHeader(s string, i int) (string, int) {
	var out strings.Builder
	var quote byte
	wordStart := true
	comment := false
	for i < len(s) {
		ch := s[i]
		if !comment && ch == '\\' && quote != '\'' && i+1 < len(s) {
			if s[i+1] != '\n' {
				out.WriteString(s[i : i+2])
				wordStart = false
			}
			i += 2
			continue
		}
		if !comment {
			if quote == 0 && ch == '#' && wordStart {
				comment = true
			}
			if quote == 0 && (ch == '\'' || ch == '"') {
				quote = ch
			} else if quote == ch {
				quote = 0
			}
		}
		out.WriteByte(ch)
		i++
		if ch == '\n' && (comment || quote == 0) {
			break
		}
		if quote == 0 {
			wordStart = strings.ContainsRune(" \t\n;|&()<>", rune(ch))
		} else {
			wordStart = false
		}
	}
	return out.String(), i
}

func stripLiteralHeredocs(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		header, next := literalHeader(s, i)
		out.WriteString(header)
		i = next
		for _, h := range literalHeredocs(header) {
			i = literalHeredocEnd(s, i, h)
		}
	}
	return out.String()
}

// Only headers are scanned for delimiter words; valid empty quoted words count.
func literalHeredocs(s string) []literalHeredoc {
	pending := []literalHeredoc{}
	wordStart := true
	for i := 0; i < len(s); {
		switch s[i] {
		case '\'', '"':
			_, i, _ = literalShellWord(s, i)
			wordStart = false
			continue
		case '\\':
			i = min(i+2, len(s))
			wordStart = false
			continue
		case '#':
			if wordStart {
				return pending
			}
		case '<':
			if strings.HasPrefix(s[i:], "<<<") {
				_, i, _ = literalShellWord(s, i+3)
				wordStart = false
				continue
			}
			if strings.HasPrefix(s[i:], "<<") {
				at := i + 2
				tabs := at < len(s) && s[at] == '-'
				if tabs {
					at++
				}
				word, next, ok := literalWord(s, at, true)
				if ok {
					quoted := strings.ContainsAny(s[at:next], "'\"\\")
					pending = append(pending, literalHeredoc{word, tabs, quoted})
				}
				i = max(i+2, next)
				wordStart = false
				continue
			}
		}
		wordStart = strings.ContainsRune(" \t\n;|&()<>", rune(s[i]))
		i++
	}
	return pending
}

func literalHeredocEnd(s string, i int, h literalHeredoc) int {
	for i < len(s) {
		var line strings.Builder
		for {
			end := strings.IndexByte(s[i:], '\n')
			nl := end >= 0
			if nl {
				end += i
			} else {
				end = len(s)
			}
			part := s[i:end]
			if h.tabs {
				part = strings.TrimLeft(part, "\t")
			}
			continued := nl && !h.quoted && strings.HasSuffix(part, "\\")
			if continued {
				part = strings.TrimSuffix(part, "\\")
			}
			line.WriteString(part)
			i = end
			if nl {
				i++
			}
			if !continued || i >= len(s) {
				break
			}
		}
		if line.String() == h.delimiter {
			return i
		}
	}
	return i
}
