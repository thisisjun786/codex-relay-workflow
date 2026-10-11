package shellir

import (
	"regexp"
	"sort"
	"strings"
)

// The writer check of a module source (CRW-1187): a source that writes, copies, moves or links a file where the interpreter looks for
// modules makes the run import text the reader did not read. A call through a writing module (os, shutil, posix, pathlib, or a name the
// source gives one), an open for writing and an archive's extract are judged by the call: when every path operand of the call is a
// string literal whose name no import reaches (a file with an extension that is not a code extension, outside __pycache__), the call
// writes no module whatever other string of the source ends .py. A call the reader cannot evaluate (an operand that is a name, an
// expression, an f-string, a concatenation, an escape sequence, a star argument, a comment inside the call, or the function taken
// without a call) keeps the rule it had: refused when the source spells a code name anywhere.

// writerSlot is one path operand of a writing call: its position, the keywords that may give it, and whether the call needs it.
type writerSlot struct {
	pos      int
	keywords []string
	required bool
}

var (
	twoPathSlots = []writerSlot{{0, []string{"src"}, true}, {1, []string{"dst"}, true}}
	openSlots    = []writerSlot{{0, []string{"path", "file"}, true}}
	extractSlots = []writerSlot{{0, []string{"member"}, true}, {1, []string{"path"}, false}}
)

// writerCallSlots are the attributes of writeAPIAttrs whose path operands name what the call writes; the others (fdopen, copyfileobj,
// unpack_archive, make_archive: a descriptor, an open file, an archive's unknown members) are never judged by their operands.
var writerCallSlots = map[string][]writerSlot{
	"copy": twoPathSlots, "copy2": twoPathSlots, "copyfile": twoPathSlots, "copytree": twoPathSlots, "move": twoPathSlots,
	"rename": twoPathSlots, "renames": twoPathSlots, "replace": twoPathSlots, "link": twoPathSlots, "symlink": twoPathSlots,
	"open": openSlots, "extract": extractSlots,
}

var (
	// reOpenWrite: open with a mode literal that writes; the operand is judged at the parenthesis.
	reOpenWrite = regexp.MustCompile(`\bopen\s*\([^)]*["'][rbtU]*[wax+][a-z+]*["']`)
	// reExtractCall: a method named extract; it writes a file for an archive the source opens, or when its operand is a code name.
	reExtractCall = regexp.MustCompile(`\.\s*extract\s*\(`)
	// reArchiveName: the source opens an archive (zipfile, tarfile) in its own text.
	reArchiveName = regexp.MustCompile(`\b(zipfile|tarfile|ZipFile|TarFile)\b`)
	// rePathLiteral: an operand that is a plain string literal (no f-string, no escape) or a path object of one.
	rePathLiteral = regexp.MustCompile(`^(?:(?:[A-Za-z_][\w.]*\.)?(?:Path|PurePath|PosixPath|PurePosixPath)\s*\(\s*((?:[rRbBuU]|[rR][bB]|[bB][rR])?(?:'[^'\\\n]*'|"[^"\\\n]*"))\s*\)|((?:[rRbBuU]|[rR][bB]|[bB][rR])?(?:'[^'\\\n]*'|"[^"\\\n]*")))$`)
	reKeywordArg  = regexp.MustCompile(`^([A-Za-z_]\w*)\s*=([^=][\s\S]*)$`)
)

// codeExtensions are the extensions of a name the interpreter may import, or that changes where it imports from, lower case.
var codeExtensions = map[string]bool{"py": true, "pyc": true, "pyo": true, "pyd": true, "pyw": true, "so": true, "pth": true, "zip": true, "egg": true}

// moduleSourceWriterRefusal is "" for a source none of whose writing calls can write a module, else what the source does.
func moduleSourceWriterRefusal(src string, aliases map[string]string) string {
	if !reCodeLiteral.MatchString(src) {
		return ""
	}
	quals := []string{"os", "shutil", "posix", "pathlib"}
	for a, mod := range aliases {
		if mod == "writer" {
			quals = append(quals, regexp.QuoteMeta(a))
		}
	}
	sort.Strings(quals[4:])
	reQual := regexp.MustCompile(`\b(` + strings.Join(quals, "|") + `)\s*\.\s*(` + strings.TrimSuffix(strings.TrimPrefix(writeAPIAttrs, "(?:"), ")") + `)\b`)
	for _, m := range reQual.FindAllStringSubmatchIndex(src, -1) {
		attr := src[m[4]:m[5]]
		if slots, ok := writerCallSlots[attr]; ok && writerCallHarmless(src, m[1], slots) {
			continue
		}
		return "writes, copies or links a file at an import location (" + src[m[2]:m[3]] + "." + attr + " with a name ending .py, .pyc, .so, .pth or in __pycache__)"
	}
	for _, m := range reOpenWrite.FindAllStringIndex(src, -1) {
		paren := m[0] + strings.IndexByte(src[m[0]:], '(')
		if !afterDot(src, m[0]) && writerCallHarmless(src, paren, openSlots) {
			continue
		}
		return "writes a file at an import location (open for writing, with a name ending .py, .pyc, .so, .pth or in __pycache__)"
	}
	archive := reArchiveName.MatchString(src)
	for _, m := range reExtractCall.FindAllStringIndex(src, -1) {
		args, ok := pyCallArgs(src, m[1]-1)
		if !ok {
			return "extracts a file the reader cannot name (extract with operands it does not read, beside a name ending .py, .pyc, .so, .pth or in __pycache__)"
		}
		for _, a := range args {
			if reCodeLiteral.MatchString(a) {
				return "extracts a file at an import location (extract of a name ending .py, .pyc, .so, .pth or in __pycache__)"
			}
		}
		if archive && !writerCallHarmless(src, m[1]-1, extractSlots) {
			return "extracts an archive member at an import location (an archive's extract with operands it does not read, beside a name ending .py, .pyc, .so, .pth or in __pycache__)"
		}
	}
	for _, m := range reWriteAPI.FindAllStringIndex(src, -1) {
		// shutil.copyfile, sh.copy2: the call through the module was judged above
		if q := attrObject(src, m[0]); q != "" && reQual.MatchString(q+"."+src[m[0]:m[1]]) {
			continue
		}
		return "writes, copies or links a file at an import location (a name ending .py, .pyc, .so, .pth or in __pycache__)"
	}
	return ""
}

// attrObject is the name whose attribute stands at i (sh for sh.copyfile), or "".
func attrObject(src string, i int) string {
	if !afterDot(src, i) {
		return ""
	}
	j := strings.LastIndexByte(src[:i], '.')
	for j > 0 && (src[j-1] == ' ' || src[j-1] == '\t') {
		j--
	}
	k := j
	for k > 0 && (isPyLetter(src[k-1]) || src[k-1] >= '0' && src[k-1] <= '9' || src[k-1] == '_') {
		k--
	}
	if k > 0 && (src[k-1] == '.' || src[k-1] >= 0x80) {
		return ""
	}
	return src[k:j]
}

// afterDot is whether the name at i is an attribute (x.open): its object, not its operand, names the file.
func afterDot(src string, i int) bool {
	for i > 0 && (src[i-1] == ' ' || src[i-1] == '\t') {
		i--
	}
	return i > 0 && src[i-1] == '.'
}

// writerCallHarmless is whether the call whose parenthesis follows i names, in every path operand, a file no import reaches.
func writerCallHarmless(src string, i int, slots []writerSlot) bool {
	args, ok := pyCallArgs(src, i)
	if !ok {
		return false
	}
	var positional []string
	keyword := map[string]string{}
	for _, a := range args {
		if strings.HasPrefix(a, "*") {
			return false
		}
		if m := reKeywordArg.FindStringSubmatch(a); m != nil {
			keyword[m[1]] = strings.TrimSpace(m[2])
			continue
		}
		positional = append(positional, a)
	}
	for _, s := range slots {
		v, found := "", false
		if s.pos < len(positional) {
			v, found = positional[s.pos], true
		}
		for _, k := range s.keywords {
			if kv, ok := keyword[k]; ok {
				if found {
					return false
				}
				v, found = kv, true
			}
		}
		if !found {
			if s.required {
				return false
			}
			continue
		}
		if !harmlessPathLiteral(v) {
			return false
		}
	}
	return true
}

// harmlessPathLiteral is whether an operand is a literal name no import reaches.
func harmlessPathLiteral(v string) bool {
	m := rePathLiteral.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	lit := m[1] + m[2]
	q := strings.IndexAny(lit, `'"`)
	name := strings.ToLower(lit[q+1 : len(lit)-1])
	if name == "" || strings.Contains(name, "__pycache__") || strings.HasSuffix(name, "/") {
		return false
	}
	base := name[strings.LastIndexByte(name, '/')+1:]
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 {
		return false
	}
	return !codeExtensions[base[dot+1:]]
}

// pyCallArgs splits the operands of the call whose parenthesis follows i (after spaces): the text of each top-level operand, trimmed.
// It is not ok when no parenthesis follows, a comment or an unclosed string or bracket stands in the call.
func pyCallArgs(src string, i int) ([]string, bool) {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	if i >= len(src) || src[i] != '(' {
		return nil, false
	}
	var args []string
	depth, start := 0, i+1
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; c {
		case '\'', '"':
			end := pyStringEnd(src, j)
			if end < 0 {
				return nil, false
			}
			j = end
		case '#':
			return nil, false
		case '(', '[', '{':
			depth++
		case ']', '}':
			depth--
		case ')':
			if depth == 0 {
				if a := strings.TrimSpace(src[start:j]); a != "" {
					args = append(args, a)
				}
				return args, true
			}
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(src[start:j]))
				start = j + 1
			}
		}
	}
	return nil, false
}

// pyStringEnd is the index of the closing quote of the string whose opening quote is at i (a triple quote too), or -1.
func pyStringEnd(src string, i int) int {
	q := src[i]
	if strings.HasPrefix(src[i:], strings.Repeat(string(q), 3)) {
		for j := i + 3; j+2 < len(src); j++ {
			if src[j] == '\\' {
				j++
				continue
			}
			if src[j] == q && src[j+1] == q && src[j+2] == q {
				return j + 2
			}
		}
		return -1
	}
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '\n':
			return -1
		case q:
			return j
		}
	}
	return -1
}
