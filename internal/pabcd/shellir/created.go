package shellir

import (
	"path"
	"strings"
)

// createdByText reports whether an earlier record of this text creates the file a program word names: ln, cp, mv and install
// write their last operand, tee all of its operands, and a redirection writes its target. The word then names what the text
// wrote (a link to a shell, a copy of one, a script), not the program its name suggests, so the reader cannot say what runs.
func (w *walker) createdByText(prog string, dir Dir) bool {
	want := createdKey(prog, dir)
	if want == "" {
		return false
	}
	for _, e := range w.out {
		for _, r := range e.Redirs {
			switch r.Op {
			case ">", ">>", ">|", "&>", "&>>":
				if r.Target.Known && createdKey(r.Target.Value, e.Dir) == want {
					return true
				}
			}
		}
		var operands []Word
		for _, a := range e.Args {
			if !a.Known || !strings.HasPrefix(a.Value, "-") || a.Value == "-" {
				operands = append(operands, a)
			}
		}
		switch e.Name {
		case "ln", "cp", "mv", "install":
			if n := len(operands); n >= 2 && operands[n-1].Known && createdKey(operands[n-1].Value, e.Dir) == want {
				return true
			}
		case "tee":
			for _, o := range operands {
				if o.Known && createdKey(o.Value, e.Dir) == want {
					return true
				}
			}
		}
	}
	return false
}

// createdKey is a path in a comparable form: relative names lose a leading ./ and keep their directory only when it is known.
func createdKey(v string, dir Dir) string {
	if v == "" || strings.HasSuffix(v, "/") {
		return ""
	}
	if !path.IsAbs(v) && dir.Known {
		v = path.Join(dir.Path, v)
	}
	return path.Clean(v)
}
