package shellir

import (
	"strconv"
	"strings"
)

// What a producer prints, as far as the text shows it, and how xargs reads it (CRW-895). The reader evaluates echo and printf
// to the exact bytes they print, so a name that printf builds from its format and its arguments (printf '../%s\n' repo) is the
// name xargs gets; a word the reader cannot evaluate makes the whole output unknown.

const (
	escEcho     = iota // echo -e (bash and dash): \0NNN is octal
	escFormat          // a printf format: \NNN is octal with one to three digits
	escPercentB        // the argument of %b: \0NNN is octal
)

// unescape decodes the backslash sequences of echo -e, a printf format or a %b argument. ok is false when a sequence is one the
// reader does not model (\c, \u, \U, an octal escape %b does not define), so the caller treats the output as unknown.
func unescape(s string, mode int) (out string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch d := s[i]; d {
		case '\\':
			b.WriteByte('\\')
		case 'a':
			b.WriteByte(7)
		case 'b':
			b.WriteByte(8)
		case 'e', 'E':
			b.WriteByte(27)
		case 'f':
			b.WriteByte(12)
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte(11)
		case 'c', 'u', 'U':
			return "", false
		case '"', '\'', '?':
			if mode == escFormat {
				b.WriteByte(d)
			} else {
				b.WriteByte('\\')
				b.WriteByte(d)
			}
		case 'x':
			j := i + 1
			for j < len(s) && j < i+3 && isHex(s[j]) {
				j++
			}
			if j == i+1 {
				b.WriteString("\\x")
				break
			}
			v, _ := strconv.ParseUint(s[i+1:j], 16, 8)
			b.WriteByte(byte(v))
			i = j - 1
		case '0', '1', '2', '3', '4', '5', '6', '7':
			start, limit := i, i+3 // one to three digits, the first of which may be the 0 of \0NNN
			if mode != escFormat {
				if d != '0' {
					if mode == escPercentB {
						return "", false
					}
					b.WriteByte('\\')
					b.WriteByte(d)
					break
				}
				start, limit = i+1, i+4 // \0 and up to three more digits
			}
			j := start
			for j < len(s) && j < limit && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(s[start:j], 8, 16)
			if start == j {
				v = 0
			}
			b.WriteByte(byte(v))
			i = j - 1
		default:
			b.WriteByte('\\')
			b.WriteByte(d)
		}
	}
	return b.String(), true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

const emitLimit = 1 << 16

// echoOutputs is what echo prints: the arguments joined by a space, and a newline unless -n. -e decodes backslash sequences, -E
// does not; with neither, bash prints them as they stand and dash decodes them, so both readings are returned.
func echoOutputs(args []Word) (outs []string, why string) {
	vals := make([]string, 0, len(args))
	for _, a := range args {
		if !a.Known {
			return nil, "the words echo prints are not known"
		}
		vals = append(vals, a.Value)
	}
	newline, interp, forceRaw := true, false, false
	for len(vals) > 0 && len(vals[0]) > 1 && vals[0][0] == '-' && strings.Trim(vals[0][1:], "neE") == "" {
		for _, c := range vals[0][1:] {
			switch c {
			case 'n':
				newline = false
			case 'e':
				interp, forceRaw = true, false
			case 'E':
				interp, forceRaw = false, true
			}
		}
		vals = vals[1:]
	}
	text := strings.Join(vals, " ")
	nl := ""
	if newline {
		nl = "\n"
	}
	switch {
	case interp:
		dec, ok := unescape(text, escEcho)
		if !ok {
			return nil, "an echo escape the reader does not model"
		}
		return []string{dec + nl}, ""
	case forceRaw || !strings.Contains(text, "\\"):
		return []string{text + nl}, ""
	}
	dec, ok := unescape(text, escEcho)
	if !ok {
		return nil, "an echo escape the reader does not model"
	}
	return []string{text + nl, dec + nl}, ""
}

type printfSeg struct {
	lit  string
	conv byte // 0 for a literal
}

// printfOutput is what printf prints for a format and its arguments. The conversions it models are %s, %b, %c, %d, %i, %u and %%
// without flags, width or precision; anything else makes the output unknown.
func printfOutput(args []Word) (string, string) {
	vals := make([]string, 0, len(args))
	for _, a := range args {
		if !a.Known {
			return "", "the words printf prints are not known"
		}
		vals = append(vals, a.Value)
	}
	if len(vals) > 0 && vals[0] == "--" {
		vals = vals[1:]
	}
	if len(vals) == 0 || strings.HasPrefix(vals[0], "-") && len(vals[0]) > 1 {
		return "", "a printf option or no format"
	}
	format, rest := vals[0], vals[1:]
	var segs []printfSeg
	var lit strings.Builder
	flush := func() bool {
		if lit.Len() == 0 {
			return true
		}
		dec, ok := unescape(lit.String(), escFormat)
		lit.Reset()
		if !ok {
			return false
		}
		segs = append(segs, printfSeg{lit: dec})
		return true
	}
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c == '\\' && i+1 < len(format) {
			lit.WriteByte(c)
			i++
			lit.WriteByte(format[i])
			continue
		}
		if c != '%' {
			lit.WriteByte(c)
			continue
		}
		i++
		if i >= len(format) {
			return "", "a printf format that ends in %"
		}
		switch format[i] {
		case '%':
			if !flush() {
				return "", "a printf escape the reader does not model"
			}
			segs = append(segs, printfSeg{lit: "%"})
		case 's', 'b', 'c', 'd', 'i', 'u':
			if !flush() {
				return "", "a printf escape the reader does not model"
			}
			segs = append(segs, printfSeg{conv: format[i]})
		default:
			return "", "a printf conversion the reader does not model"
		}
	}
	if !flush() {
		return "", "a printf escape the reader does not model"
	}
	convs := 0
	for _, s := range segs {
		if s.conv != 0 {
			convs++
		}
	}
	var out strings.Builder
	next := 0
	for pass := 0; pass == 0 || convs > 0 && next < len(rest); pass++ {
		for _, s := range segs {
			if s.conv == 0 {
				out.WriteString(s.lit)
				continue
			}
			arg := ""
			if next < len(rest) {
				arg = rest[next]
				next++
			} else if s.conv == 'd' || s.conv == 'i' || s.conv == 'u' {
				arg = "0"
			}
			switch s.conv {
			case 's':
				out.WriteString(arg)
			case 'b':
				dec, ok := unescape(arg, escPercentB)
				if !ok {
					return "", "a %b escape the reader does not model"
				}
				out.WriteString(dec)
			case 'c':
				if arg != "" {
					out.WriteString(arg[:1])
				}
			default:
				n, ok := plainInteger(arg)
				if !ok || s.conv == 'u' && n < 0 {
					return "", "a printf number the reader does not model"
				}
				out.WriteString(strconv.FormatInt(n, 10))
			}
			if out.Len() > emitLimit {
				return "", "printf output is too long to read"
			}
		}
	}
	return out.String(), ""
}

func plainInteger(s string) (int64, bool) {
	t := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	if t == "" || len(t) > 1 && t[0] == '0' {
		return 0, false
	}
	for _, c := range t {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// xargsOpts are the options of xargs that decide what operands it builds from standard input.
type xargsOpts struct {
	ArgFile  bool   // -a FILE: the operands come from a file
	Null     bool   // -0
	Delim    string // -d CHAR
	DelimSet bool
	Replace  string // -I STR, -i[STR]: the string the command's words replace by each input line
}

// xargsItems are the operands xargs builds from the texts a producer may print. Every candidate the producer's text can yield
// is returned (a line-selecting filter in front can leave any line out, so each line is a candidate of its own, and a blank-trimmed
// variant of each is added), so a judgment over them does not depend on which one xargs reads.
func xargsItems(src *pipeSource, o xargsOpts) ([]string, string) {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		for _, v := range []string{s, strings.TrimSpace(s)} {
			if v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	for _, text := range src.outs {
		texts := []string{text}
		if src.sub {
			texts = append(texts, strings.Split(text, "\n")...)
		}
		for _, t := range texts {
			switch {
			case o.DelimSet:
				for _, p := range strings.Split(t, o.Delim) {
					add(p)
				}
			case o.Null:
				for _, p := range strings.Split(t, "\x00") {
					add(p)
				}
			case o.Replace != "":
				for _, line := range strings.Split(t, "\n") {
					line = strings.TrimLeft(line, " \t")
					add(line)
					add(xargsUnquote(line))
				}
			default:
				for _, tok := range xargsTokens(t) {
					add(tok)
				}
			}
		}
	}
	if len(out) > 512 {
		return nil, "the names xargs reads are too many to judge"
	}
	return out, ""
}

// xargsTokens splits input as xargs does without -0, -d or -I: blanks and newlines separate the items, single and double quotes
// group, and a backslash outside quotes keeps the next character. An unmatched quote is an error to xargs; the rest of the input
// is then one item here, so it is still judged.
func xargsTokens(s string) []string {
	var out []string
	var cur strings.Builder
	has := false
	end := func() {
		if has {
			out = append(out, cur.String())
		}
		cur.Reset()
		has = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' || c == 0:
			end()
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			has = true
		case c == '\'' || c == '"':
			has = true
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				cur.WriteString(s[i+1:])
				i = len(s)
				break
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	end()
	return out
}

// xargsUnquote removes the quotes and backslashes xargs removes from one line when it keeps the blanks (-I).
func xargsUnquote(line string) string {
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case c == '\'' || c == '"':
			j := strings.IndexByte(line[i+1:], c)
			if j < 0 {
				cur.WriteString(line[i+1:])
				return cur.String()
			}
			cur.WriteString(line[i+1 : i+1+j])
			i += j + 1
		default:
			cur.WriteByte(c)
		}
	}
	return cur.String()
}
