package shellir

import (
	"slices"
	"strings"
	"testing"
)

func optWords(ss ...string) []Word {
	out := make([]Word, len(ss))
	for i, s := range ss {
		out[i] = Word{Known: true, Value: s}
	}
	return out
}

func optValues(ws []Word) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Value)
	}
	return out
}

// TestMatchLongOption: a long option name is read the way getopt_long reads it.
func TestMatchLongOption(t *testing.T) {
	for _, c := range []struct {
		table []LongOption
		name  string
		want  string // the option found, or "none" or "ambiguous"
	}{
		{SortLongOptions(), "output", "output"},
		{SortLongOptions(), "o", "output"},
		{SortLongOptions(), "ou", "output"},
		{SortLongOptions(), "outp", "output"},
		{SortLongOptions(), "outpu", "output"},
		{SortLongOptions(), "s", "ambiguous"},     // sort, stable
		{SortLongOptions(), "so", "sort"},         // only --sort
		{SortLongOptions(), "st", "stable"},       // only --stable
		{SortLongOptions(), "c", "ambiguous"},     // check, compress-program
		{SortLongOptions(), "ch", "check"},        // only --check
		{SortLongOptions(), "version", "version"}, // an exact name wins over --version-sort
		{SortLongOptions(), "v", "ambiguous"},     // version, version-sort
		{SortLongOptions(), "x", "none"},
		{SortLongOptions(), "outputs", "none"},
		{SortLongOptions(), "", "ambiguous"},
		{SedLongOptions(), "e", "expression"},
		{SedLongOptions(), "expr", "expression"},
		{SedLongOptions(), "in", "in-place"},
		{SedLongOptions(), "i", "in-place"},
		{SedLongOptions(), "f", "ambiguous"}, // file, follow-symlinks
		{SedLongOptions(), "fi", "file"},
		{SedLongOptions(), "fo", "follow-symlinks"},
		{SedLongOptions(), "s", "ambiguous"}, // sandbox, separate, silent
		{SedLongOptions(), "z", "zero-terminated"},
		{SedLongOptions(), "n", "null-data"},
		{SedLongOptions(), "q", "quiet"},
		{SedLongOptions(), "li", "line-length"},
	} {
		opt, m := MatchLongOption(c.table, c.name)
		got := opt.Name
		switch m {
		case LongNone:
			got = "none"
		case LongAmbiguous:
			got = "ambiguous"
		}
		if got != c.want {
			t.Errorf("--%s reads as %s, want %s", c.name, got, c.want)
		}
	}
	// Names that begin the same and mean the same are one option to getopt_long.
	same := []LongOption{{"alpha", OptionNoArg, "a"}, {"alpine", OptionNoArg, "a"}, {"beta", OptionNoArg, "b"}}
	if o, m := MatchLongOption(same, "al"); m != LongFound || o.Key != "a" {
		t.Errorf("two names of one meaning must read as that option: %+v %v", o, m)
	}
	diff := []LongOption{{"alpha", OptionNoArg, "a"}, {"alpine", OptionRequiredArg, "a"}}
	if _, m := MatchLongOption(diff, "al"); m != LongAmbiguous {
		t.Errorf("names that differ in their value must be ambiguous: %v", m)
	}
}

// TestParseSedArgs reads sed's command line.
func TestParseSedArgs(t *testing.T) {
	type want struct {
		scripts, files, operands []string
		inPlace                  bool
	}
	for _, c := range []struct {
		args []string
		want want
	}{
		{[]string{"p", "x"}, want{operands: []string{"p", "x"}}},
		{[]string{"-n", "-e", "p", "-e", "q", "x"}, want{scripts: []string{"p", "q"}, operands: []string{"x"}}},
		{[]string{"-ne", "p", "x"}, want{scripts: []string{"p"}, operands: []string{"x"}}},
		{[]string{"-ep", "x"}, want{scripts: []string{"p"}, operands: []string{"x"}}},
		{[]string{"--expression", "p", "x"}, want{scripts: []string{"p"}, operands: []string{"x"}}},
		{[]string{"--expression=p", "x"}, want{scripts: []string{"p"}, operands: []string{"x"}}},
		{[]string{"--expr=p", "x"}, want{scripts: []string{"p"}, operands: []string{"x"}}},
		{[]string{"--e", "p", "x"}, want{scripts: []string{"p"}, operands: []string{"x"}}},
		{[]string{"--exp=p", "--exp", "q"}, want{scripts: []string{"p", "q"}}},
		{[]string{"--file", "s.sed", "x"}, want{files: []string{"s.sed"}, operands: []string{"x"}}},
		{[]string{"--fi=s.sed", "x"}, want{files: []string{"s.sed"}, operands: []string{"x"}}},
		{[]string{"-nf", "s.sed"}, want{files: []string{"s.sed"}}},
		{[]string{"-f-", "x"}, want{files: []string{"-"}, operands: []string{"x"}}},
		{[]string{"-i", "s/a/b/", "f"}, want{inPlace: true, operands: []string{"s/a/b/", "f"}}},
		{[]string{"-ni", "p", "f"}, want{inPlace: true, operands: []string{"p", "f"}}},
		{[]string{"-i.bak", "p", "f"}, want{inPlace: true, operands: []string{"p", "f"}}},
		{[]string{"--in-place", "p", "f"}, want{inPlace: true, operands: []string{"p", "f"}}},
		{[]string{"--in", "p", "f"}, want{inPlace: true, operands: []string{"p", "f"}}},
		{[]string{"--i=.bak", "p", "f"}, want{inPlace: true, operands: []string{"p", "f"}}},
		{[]string{"p", "f", "-i"}, want{inPlace: true, operands: []string{"p", "f"}}},
		{[]string{"-l", "5", "p"}, want{operands: []string{"p"}}},
		{[]string{"-l5", "p"}, want{operands: []string{"p"}}},
		{[]string{"--line-length", "5", "p"}, want{operands: []string{"p"}}},
		{[]string{"--line-length=5", "p"}, want{operands: []string{"p"}}},
		{[]string{"--line", "5", "p"}, want{operands: []string{"p"}}},
		{[]string{"-s", "-u", "-z", "-E", "-r", "-n", "p"}, want{operands: []string{"p"}}},
		{[]string{"--quiet", "--silent", "--regexp-extended", "--null-data", "--zero-terminated", "--separate", "--unbuffered", "--posix", "--debug", "--sandbox", "p"}, want{operands: []string{"p"}}},
		{[]string{"--", "-n", "x"}, want{operands: []string{"-n", "x"}}},
		{[]string{"-n", "--", "p", "-i"}, want{operands: []string{"p", "-i"}}},
		{[]string{"p", "-"}, want{operands: []string{"p", "-"}}},
	} {
		pa, err := ParseSedArgs("sed", optWords(c.args...))
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		got := want{optValues(pa.Scripts), optValues(pa.Files), optValues(pa.Operands), pa.InPlace}
		if !slices.Equal(got.scripts, c.want.scripts) || !slices.Equal(got.files, c.want.files) || !slices.Equal(got.operands, c.want.operands) || got.inPlace != c.want.inPlace {
			t.Errorf("%q read as %+v, want %+v", c.args, got, c.want)
		}
	}
}

// TestParseSedArgsRefusesWhatItCannotProve: an option the reader does not model, an ambiguous abbreviation, a missing or
// misplaced value and an option word the reader cannot evaluate are unreadable.
func TestParseSedArgsRefusesWhatItCannotProve(t *testing.T) {
	for _, args := range [][]string{
		{"--f", "s.sed", "x"},       // file or follow-symlinks
		{"--s", "p", "x"},           // sandbox, separate or silent
		{"--no-such-option", "p"},   // unknown
		{"--version"},               // prints and exits
		{"--help"},                  // prints and exits
		{"--v"},                     // --version
		{"-V"},                      // unknown
		{"-o", "p"},                 // not a sed option
		{"-e"},                      // no value
		{"--expression"},            // no value
		{"--file"},                  // no value
		{"--quiet=yes", "p"},        // an option that takes no value
		{"--expression=p", "--f=s"}, // ambiguous after a good one
	} {
		if _, err := ParseSedArgs("sed", optWords(args...)); err == nil {
			t.Errorf("%q must be unreadable", args)
		} else if !strings.Contains(err.Error(), "sed") {
			t.Errorf("%q: the reason does not name the program: %v", args, err)
		}
	}
	// An option word that depends on something the reader cannot see, before the first operand.
	args := []Word{{Known: false, Reason: "variable"}, {Known: true, Value: "p"}}
	if _, err := ParseSedArgs("sed", args); err == nil {
		t.Error("an unknown word in the option position must be unreadable")
	}
	// After the first operand an unknown word is a file operand.
	args = []Word{{Known: true, Value: "p"}, {Known: false, Reason: "variable"}}
	if pa, err := ParseSedArgs("sed", args); err != nil || len(pa.Operands) != 2 {
		t.Errorf("an unknown file operand: %+v %v", pa, err)
	}
}
