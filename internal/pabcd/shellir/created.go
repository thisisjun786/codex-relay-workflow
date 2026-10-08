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
	if w.created == nil {
		w.created = map[string]bool{}
	}
	for ; w.createdUpTo < len(w.out); w.createdUpTo++ {
		w.noteCreated(w.out[w.createdUpTo])
	}
	return w.created[want]
}

// noteCreated adds the files one record writes to the created set.
func (w *walker) noteCreated(e Exec) {
	add := func(v string, d Dir) {
		if key := createdKey(v, d); key != "" {
			w.created[key] = true
		}
	}
	for _, r := range e.Redirs {
		switch r.Op {
		case ">", ">>", ">|", "&>", "&>>":
			if r.Target.Known {
				add(r.Target.Value, e.Dir)
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
		if n := len(operands); n >= 2 && operands[n-1].Known {
			add(operands[n-1].Value, e.Dir)
		}
	case "tee":
		for _, o := range operands {
			if o.Known {
				add(o.Value, e.Dir)
			}
		}
	}
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
