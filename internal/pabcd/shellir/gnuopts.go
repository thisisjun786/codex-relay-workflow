package shellir

import "strings"

// OptionArg says whether a GNU option takes a value: none, a required one (attached with = or =VALUE for a long option, the
// next word otherwise), or an optional one (attached only).
type OptionArg int

const (
	OptionNoArg OptionArg = iota
	OptionRequiredArg
	OptionOptionalArg
)

// LongOption is one entry of a getopt_long table. Key names what the option means: two names (or a long name and a short
// letter) that mean the same thing share a Key, which is what getopt_long compares when it decides an abbreviation is
// unambiguous.
type LongOption struct {
	Name string
	Arg  OptionArg
	Key  string
}

// LongMatch is the outcome of reading a long option name against a table.
type LongMatch int

const (
	// LongNone: no option of the table begins with the name.
	LongNone LongMatch = iota
	// LongFound: the name is an option, spelled in full or as the only prefix that matches.
	LongFound
	// LongAmbiguous: several options with different meanings begin with the name; getopt_long stops the program.
	LongAmbiguous
)

// MatchLongOption reads a long option name (the text after the two dashes, without any =VALUE) the way getopt_long does: a
// name that is exactly an option is that option; otherwise a name that is a prefix of exactly one option, or of several that
// all take the same kind of value and mean the same thing, is that option; a prefix of options that differ is ambiguous.
func MatchLongOption(table []LongOption, name string) (LongOption, LongMatch) {
	var found []LongOption
	for _, o := range table {
		if o.Name == name {
			return o, LongFound
		}
		if strings.HasPrefix(o.Name, name) {
			found = append(found, o)
		}
	}
	switch len(found) {
	case 0:
		return LongOption{}, LongNone
	case 1:
		return found[0], LongFound
	}
	for _, o := range found[1:] {
		if o.Arg != found[0].Arg || o.Key != found[0].Key {
			return LongOption{}, LongAmbiguous
		}
	}
	return found[0], LongFound
}

// SortLongOptions is the long option table of GNU sort (coreutils sort.c). The destination of the write sort makes is the
// value of the option with Key "o".
func SortLongOptions() []LongOption {
	return []LongOption{
		{"batch-size", OptionRequiredArg, "batch-size"},
		{"buffer-size", OptionRequiredArg, "S"},
		{"check", OptionOptionalArg, "check"},
		{"compress-program", OptionRequiredArg, "compress-program"},
		{"debug", OptionNoArg, "debug"},
		{"dictionary-order", OptionNoArg, "d"},
		{"field-separator", OptionRequiredArg, "t"},
		{"files0-from", OptionRequiredArg, "files0-from"},
		{"general-numeric-sort", OptionNoArg, "g"},
		{"help", OptionNoArg, "help"},
		{"human-numeric-sort", OptionNoArg, "h"},
		{"ignore-case", OptionNoArg, "f"},
		{"ignore-leading-blanks", OptionNoArg, "b"},
		{"ignore-nonprinting", OptionNoArg, "i"},
		{"key", OptionRequiredArg, "k"},
		{"merge", OptionNoArg, "m"},
		{"month-sort", OptionNoArg, "M"},
		{"numeric-sort", OptionNoArg, "n"},
		{"output", OptionRequiredArg, "o"},
		{"parallel", OptionRequiredArg, "parallel"},
		{"random-sort", OptionNoArg, "R"},
		{"random-source", OptionRequiredArg, "random-source"},
		{"reverse", OptionNoArg, "r"},
		{"sort", OptionRequiredArg, "sort"},
		{"stable", OptionNoArg, "s"},
		{"temporary-directory", OptionRequiredArg, "T"},
		{"unique", OptionNoArg, "u"},
		{"version", OptionNoArg, "version"},
		{"version-sort", OptionNoArg, "V"},
		{"zero-terminated", OptionNoArg, "z"},
	}
}

// SortShortValueOptions are the short options of GNU sort that take a value, attached or as the next word.
const SortShortValueOptions = "koSTty"

// SedLongOptions is the long option table of GNU sed (sed.c).
func SedLongOptions() []LongOption {
	return []LongOption{
		{"binary", OptionNoArg, "b"},
		{"debug", OptionNoArg, "debug"},
		{"expression", OptionRequiredArg, "e"},
		{"file", OptionRequiredArg, "f"},
		{"follow-symlinks", OptionNoArg, "follow-symlinks"},
		{"help", OptionNoArg, "help"},
		{"in-place", OptionOptionalArg, "i"},
		{"line-length", OptionRequiredArg, "l"},
		{"null-data", OptionNoArg, "z"},
		{"posix", OptionNoArg, "posix"},
		{"quiet", OptionNoArg, "n"},
		{"regexp-extended", OptionNoArg, "E"},
		{"sandbox", OptionNoArg, "sandbox"},
		{"separate", OptionNoArg, "s"},
		{"silent", OptionNoArg, "n"},
		{"unbuffered", OptionNoArg, "u"},
		{"version", OptionNoArg, "version"},
		{"zero-terminated", OptionNoArg, "z"},
	}
}

// SedArgs is what sed's command line holds, read the way getopt_long reads it: options may follow operands, -- ends them,
// and a long option may be abbreviated to any unambiguous prefix.
type SedArgs struct {
	// Scripts are the values of -e and --expression, in order.
	Scripts []Word
	// Files are the values of -f and --file.
	Files []Word
	// InPlace is set by -i, -I and --in-place (in any spelling).
	InPlace bool
	// InPlaceSuffix is the backup suffix of the last -i, -I or --in-place (empty: no backup). GNU sed replaces each * of it by
	// the name of the input file and may move the backup into another directory.
	InPlaceSuffix string
	// PosixScript is the first operand when a script option follows it. getopt_long permutes the arguments, so that operand is
	// a file and the option gives the script; with POSIXLY_CORRECT set (which the text, the wrapper or the session
	// environment can do, and the reader cannot prove it does not) getopt stops at the first operand: it is the script that
	// runs and the later options are files. Both readings are judged, so a script the other reading would run is not hidden.
	PosixScript *Word
	// PosixTail, PosixInPlace and PosixInPlaceSuffix are the other reading of the same command line, set only when an option
	// follows the first operand (so the two readings differ). With POSIXLY_CORRECT getopt stops at the first operand: PosixTail
	// is every word from it on, all of them operands (an option-looking word is then a file name), and only the -i, -I and
	// --in-place before it count (PosixInPlace, PosixInPlaceSuffix). A later -i does not replace the suffix of an earlier one
	// there, so the in-place places of both readings are judged.
	PosixTail          []Word
	PosixInPlace       bool
	PosixInPlaceSuffix string
	// Operands are the words that are no option or option value. When neither -e nor -f gives a script, the first operand is the
	// script.
	Operands []Word
}

// ParseSedArgs reads the arguments of sed. A word the reader cannot evaluate before the first operand, an option the reader
// does not model (including --help and --version, which run no script), an ambiguous abbreviation, an option that needs a
// value and has none, and a value given to an option that takes none are unreadable: what runs is not proven.
func ParseSedArgs(name string, args []Word) (SedArgs, error) {
	var pa SedArgs
	longs := SedLongOptions()
	optionsEnd := false
	scriptBeforeOperand := false
	firstOperand := -1
	lateOption := false
	var posixInPlace bool
	var posixSuffix string
	setSuffix := func(s string) {
		pa.InPlace = true
		pa.InPlaceSuffix = s
		if len(pa.Operands) == 0 {
			posixInPlace, posixSuffix = true, s
		}
	}
	for i := 0; i < len(args); {
		a := args[i]
		if !a.Known {
			if len(pa.Operands) == 0 && !optionsEnd {
				return SedArgs{}, unreadablef("%s option is not known (%s)", name, a.Reason)
			}
			if len(pa.Operands) == 0 {
				scriptBeforeOperand = len(pa.Scripts)+len(pa.Files) > 0
				firstOperand = i
			}
			pa.Operands = append(pa.Operands, a)
			i++
			continue
		}
		v := a.Value
		switch {
		case optionsEnd || v == "-" || !strings.HasPrefix(v, "-"):
			if len(pa.Operands) == 0 {
				scriptBeforeOperand = len(pa.Scripts)+len(pa.Files) > 0
				firstOperand = i
			}
			pa.Operands = append(pa.Operands, a)
			i++
			continue
		case v == "--":
			optionsEnd = true
			lateOption = lateOption || len(pa.Operands) > 0
			i++
			continue
		}
		i++
		lateOption = lateOption || len(pa.Operands) > 0
		if strings.HasPrefix(v, "--") {
			optName, attached, hasValue := strings.Cut(v[2:], "=")
			opt, match := MatchLongOption(longs, optName)
			switch match {
			case LongNone:
				return SedArgs{}, unreadablef("%s option %s is not modelled", name, v)
			case LongAmbiguous:
				return SedArgs{}, unreadablef("%s option %s is an ambiguous abbreviation", name, v)
			}
			var val Word
			switch opt.Arg {
			case OptionNoArg:
				if hasValue {
					return SedArgs{}, unreadablef("%s option %s takes no value", name, v)
				}
			case OptionRequiredArg:
				if hasValue {
					val = Word{Known: true, Value: attached}
				} else {
					if i >= len(args) {
						return SedArgs{}, unreadablef("%s option %s without a value", name, v)
					}
					val = args[i]
					i++
				}
			}
			if opt.Key == "i" {
				setSuffix(attached)
			}
			if pa.lateScript(opt.Key, scriptBeforeOperand) {
				pa.PosixScript = &pa.Operands[0]
			}
			if err := pa.apply(name, opt.Key, val, v); err != nil {
				return SedArgs{}, err
			}
			continue
		}
		for k := 1; k < len(v); k++ {
			c := v[k]
			switch c {
			case 'n', 'E', 'r', 's', 'u', 'z', 'b':
			case 'i', 'I':
				setSuffix(v[k+1:]) // the rest of the word is the suffix
				k = len(v)
			case 'e', 'f', 'l':
				var val Word
				if k < len(v)-1 {
					val = Word{Known: true, Value: v[k+1:]}
				} else {
					if i >= len(args) {
						return SedArgs{}, unreadablef("%s -%c without a value", name, c)
					}
					val = args[i]
					i++
				}
				if pa.lateScript(string(c), scriptBeforeOperand) {
					pa.PosixScript = &pa.Operands[0]
				}
				if err := pa.apply(name, string(c), val, v); err != nil {
					return SedArgs{}, err
				}
				k = len(v)
			default:
				return SedArgs{}, unreadablef("%s option -%c is not modelled", name, c)
			}
		}
	}
	if lateOption && firstOperand >= 0 {
		pa.PosixTail = args[firstOperand:]
		pa.PosixInPlace, pa.PosixInPlaceSuffix = posixInPlace, posixSuffix
	}
	return pa, nil
}

// lateScript is whether the option with this Key is a script option that follows the first operand when no script option
// came before it: the case where getopt_long with and without POSIXLY_CORRECT disagree about which word is the script.
func (pa *SedArgs) lateScript(key string, scriptBeforeOperand bool) bool {
	return (key == "e" || key == "f") && len(pa.Operands) > 0 && !scriptBeforeOperand && pa.PosixScript == nil
}

// apply records one option by its Key.
func (pa *SedArgs) apply(name, key string, val Word, spelled string) error {
	switch key {
	case "e":
		pa.Scripts = append(pa.Scripts, val)
	case "f":
		pa.Files = append(pa.Files, val)
	case "i":
		pa.InPlace = true
	case "n", "E", "s", "u", "z", "b", "l", "debug", "posix", "sandbox", "follow-symlinks":
	default:
		// --help and --version print and exit: no script runs, and the reader does not model them.
		return unreadablef("%s option %s is not modelled", name, spelled)
	}
	return nil
}
