package hook

import "unicode"

// memoryRequestUnquote excludes complete or unfinished quote/code spans. An
// apostrophe inside a word (don't) is prose; Markdown code delimiters are runs,
// so a double-backtick example cannot leak through two empty single spans.
func memoryRequestUnquote(s string) string {
	r := []rune(s)
	out := make([]rune, 0, len(r))
	for i := 0; i < len(r); {
		c := r[i]
		if c != '\'' && c != '"' && c != '`' && c != '“' && c != '‘' {
			out = append(out, c)
			i++
			continue
		}
		if c == '\'' && i > 0 && (unicode.IsLetter(r[i-1]) || unicode.IsDigit(r[i-1])) {
			out = append(out, c)
			i++
			continue
		}
		end := c
		if c == '“' {
			end = '”'
		}
		if c == '‘' {
			end = '’'
		}
		width := 1
		if c == '`' {
			for i+width < len(r) && r[i+width] == c {
				width++
			}
		}
		i += width
		for i < len(r) {
			if c != '`' && r[i] == '\\' {
				i += min(2, len(r)-i)
				continue
			}
			if r[i] == end {
				n := 1
				for i+n < len(r) && r[i+n] == end {
					n++
				}
				if c != '`' || n == width {
					i += min(width, n)
					break
				}
				i += n
				continue
			}
			i++
		}
		out = append(out, ' ')
	}
	return string(out)
}
