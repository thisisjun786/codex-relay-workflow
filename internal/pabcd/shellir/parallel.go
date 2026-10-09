package shellir

import (
	"strconv"
	"strings"
)

// maxParallelCombinations bounds the command lines one parallel text makes; a larger set is unreadable.
const maxParallelCombinations = 64

// parallelSource is one ::: source: its values, and whether it was introduced by :::+ (zipped with the source before it).
type parallelSource struct {
	values []string
	linked bool
}

// parallelUnwrap models GNU parallel: its options, its command template and its ::: sources. Each combination of source
// values makes one command line. GNU joins the template words with blanks and runs the line in a shell, so the reader
// runs the same text: the expanded lines are carried like a shell string. A source the grammar does not list, a remote
// host, a file of arguments, a value the shell would split or quote, a placeholder the reader does not model, or a
// template whose program is a placeholder leaves the program unproven.
func parallelUnwrap(args []Word) (unwrapped, error) {
	var u unwrapped
	i := 0
	for i < len(args) {
		v, err := knownValue(args[i], "parallel option")
		if err != nil {
			return u, err
		}
		if v == "--" {
			i++
			break
		}
		if len(v) < 2 || v[0] != '-' {
			break
		}
		if v == "-j" || v == "--jobs" {
			if i+1 >= len(args) {
				return u, unreadablef("parallel %s without a value", v)
			}
			if _, err := knownValue(args[i+1], "parallel job count"); err != nil {
				return u, err
			}
			i += 2
			continue
		}
		if isParallelJobCount(v) || parallelPlainOption(v) {
			i++
			continue
		}
		return u, unreadablef("parallel option %s is not modelled", v)
	}

	j := i
	for j < len(args) && !parallelMarker(args[j]) {
		j++
	}
	template := args[i:j]
	if len(template) == 0 {
		return u, unreadablef("parallel without a command template")
	}
	if j == len(args) {
		return u, unreadablef("parallel without a ::: source reads its arguments from standard input")
	}
	words := make([]string, len(template))
	for k, w := range template {
		v, err := knownValue(w, "parallel template word")
		if err != nil {
			return u, err
		}
		if strings.Contains(v, "{=") || strings.Contains(v, "{+") || strings.Contains(v, "{%") {
			return u, unreadablef("parallel placeholder in %q is not modelled", v)
		}
		words[k] = v
	}
	if strings.Contains(words[0], "{") {
		return u, unreadablef("parallel runs a program its template names through a placeholder")
	}

	var sources []parallelSource
	for j < len(args) {
		marker := args[j].Value
		if marker == "::::" || marker == "::::+" {
			return u, unreadablef("parallel reads its sources from a file")
		}
		src := parallelSource{linked: marker == ":::+"}
		j++
		for j < len(args) && !parallelMarker(args[j]) {
			v, err := knownValue(args[j], "parallel source value")
			if err != nil {
				return u, err
			}
			src.values = append(src.values, v)
			j++
		}
		if len(src.values) == 0 {
			return u, unreadablef("parallel source is empty")
		}
		sources = append(sources, src)
	}

	// A :::+ source is zipped with the source before it; each run of zipped sources is one factor of the product.
	var bundles [][]int
	for s, src := range sources {
		if !src.linked {
			bundles = append(bundles, []int{s})
			continue
		}
		if len(bundles) == 0 {
			return u, unreadablef("parallel :::+ has no source before it")
		}
		last := len(bundles) - 1
		bundles[last] = append(bundles[last], s)
	}
	combos := [][]string{make([]string, len(sources))}
	for _, b := range bundles {
		n := len(sources[b[0]].values)
		for _, s := range b {
			if len(sources[s].values) != n {
				return u, unreadablef("parallel linked sources have different lengths")
			}
		}
		if len(combos)*n > maxParallelCombinations {
			return u, unreadablef("parallel makes more than %d command lines", maxParallelCombinations)
		}
		var next [][]string
		for _, c := range combos {
			for k := 0; k < n; k++ {
				row := append([]string(nil), c...)
				for _, s := range b {
					row[s] = sources[s].values[k]
				}
				next = append(next, row)
			}
		}
		combos = next
	}

	lines := make([]string, 0, len(combos))
	for seq, row := range combos {
		out := make([]string, 0, len(words)+len(row))
		placeheld := false
		for _, w := range words {
			expanded, used, err := expandParallelWord(w, row, seq+1)
			if err != nil {
				return u, err
			}
			placeheld = placeheld || used
			out = append(out, expanded)
		}
		if !placeheld {
			// GNU appends the source values to a template that names no placeholder.
			for _, v := range row {
				if !parallelPlainValue(v) {
					return u, unreadablef("parallel argument %q is not a plain word", v)
				}
				out = append(out, v)
			}
		}
		lines = append(lines, strings.Join(out, " "))
	}
	u.isShell = true
	u.shell = strings.Join(lines, "\n")
	u.shellCarrier = "parallel"
	return u, nil
}

// expandParallelWord replaces the placeholders of one template word with the values of one combination. seq is the
// job's sequence number ({#}). It reports whether the word named a placeholder.
func expandParallelWord(word string, row []string, seq int) (string, bool, error) {
	var b strings.Builder
	used := false
	for i := 0; i < len(word); {
		if word[i] != '{' {
			b.WriteByte(word[i])
			i++
			continue
		}
		end := strings.IndexByte(word[i:], '}')
		if end < 0 {
			b.WriteString(word[i:])
			break
		}
		inner := word[i+1 : i+end]
		i += end + 1
		if inner == "#" {
			b.WriteString(strconv.Itoa(seq))
			used = true
			continue
		}
		if strings.HasPrefix(inner, "=") || strings.HasPrefix(inner, "%") || strings.HasPrefix(inner, "+") {
			return "", false, unreadablef("parallel placeholder {%s} is not modelled", inner)
		}
		digits := 0
		for digits < len(inner) && inner[digits] >= '0' && inner[digits] <= '9' {
			digits++
		}
		mod := inner[digits:]
		switch mod {
		case "", ".", "/", "//", "/.":
		default:
			// Not a placeholder: a brace that the shell keeps as it is.
			b.WriteString("{" + inner + "}")
			continue
		}
		idx := 0
		if digits > 0 {
			n, _ := strconv.Atoi(inner[:digits])
			if n < 1 || n > len(row) {
				return "", false, unreadablef("parallel placeholder {%s} names no source", inner)
			}
			idx = n - 1
		} else if len(row) != 1 {
			return "", false, unreadablef("parallel placeholder {} with %d sources is not modelled", len(row))
		}
		value := parallelTransform(row[idx], mod)
		if !parallelPlainValue(value) {
			return "", false, unreadablef("parallel argument %q is not a plain word", value)
		}
		b.WriteString(value)
		used = true
	}
	return b.String(), used, nil
}

// parallelTransform applies the path modifier of a placeholder: none, . (no extension), / (basename), // (dirname) or
// /. (basename without extension), as GNU parallel does.
func parallelTransform(v, mod string) string {
	base := v
	if slash := strings.LastIndexByte(v, '/'); slash >= 0 {
		base = v[slash+1:]
	}
	noExt := func(s string) string {
		if dot := strings.LastIndexByte(s, '.'); dot > 0 {
			return s[:dot]
		}
		return s
	}
	switch mod {
	case ".":
		return noExt(v)
	case "/":
		return base
	case "//":
		slash := strings.LastIndexByte(v, '/')
		switch {
		case slash < 0:
			return "."
		case slash == 0:
			return "/"
		}
		return v[:slash]
	case "/.":
		return noExt(base)
	}
	return v
}

// parallelPlainValue reports a value the shell reads as one word without any quoting: letters, digits and a few punctuation
// marks. Any other value would make the expanded line depend on quoting the reader does not follow.
func parallelPlainValue(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("_./:@%+=,-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func parallelMarker(w Word) bool {
	return w.Known && (w.Value == ":::" || w.Value == "::::" || w.Value == ":::+" || w.Value == "::::+")
}

// parallelPlainOption lists the options that change no command line and run nothing: the output and progress options.
func parallelPlainOption(v string) bool {
	switch v {
	case "-k", "-t", "-v", "-u", "--keep-order", "--tag", "--verbose", "--ungroup", "--no-notice", "--bar", "--progress":
		return true
	}
	return false
}

// isParallelJobCount reports the attached job count of -jN or --jobs=N.
func isParallelJobCount(v string) bool {
	var rest string
	switch {
	case strings.HasPrefix(v, "-j"):
		rest = v[2:]
	case strings.HasPrefix(v, "--jobs="):
		rest = v[len("--jobs="):]
	default:
		return false
	}
	return rest != "" && allDigits(rest)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
