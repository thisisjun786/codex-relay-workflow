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
		w.createdTrees = map[string]bool{}
	}
	for ; w.createdUpTo < len(w.out); w.createdUpTo++ {
		w.noteCreated(w.out[w.createdUpTo])
	}
	if w.created[want] {
		return true
	}
	// A copied directory (cp -r evil bin) creates every file under bin/evil.
	for p := want; ; {
		if w.createdTrees[p] {
			return true
		}
		up := path.Dir(p)
		if up == p {
			return false
		}
		p = up
	}
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
	switch e.Name {
	case "ln", "cp", "mv", "install":
		w.noteCopied(e, add)
	case "tee":
		for _, a := range e.Args {
			if a.Known && (!strings.HasPrefix(a.Value, "-") || a.Value == "-") {
				add(a.Value, e.Dir)
			}
		}
	}
}

// copyValuedOptions are the short options of ln, cp, mv and install that take a value (a suffix, a mode, an owner, a group, a
// target directory).
const copyValuedOptions = "tSmog"

// noteCopied adds the files and trees ln, cp, mv and install write to the created sets.
func (w *walker) noteCopied(e Exec, add func(string, Dir)) {
	files, trees := CopiedPaths(e)
	for _, f := range files {
		add(f, e.Dir)
	}
	for _, t := range trees {
		if key := createdKey(t, e.Dir); key != "" {
			w.createdTrees[key] = true
		}
	}
}

// CopiedPaths returns what a ln, cp, mv or install record writes, as the operands spell them (relative to the record's
// directory): the files, and the trees, a path under which may be written too. With -t or --target-directory every source
// operand is written into that directory under its own name; with -T the last operand is the file; otherwise the last operand
// is a file, or a directory the sources are written into when it is one: the reader cannot say which, so both are recorded
// (the last operand as a file, and each source name below it as the tree a copied directory fills). A destination that a
// directory source may become itself (cp -r evil bin, cp -rT evil bin, mv evil bin, ln -s evildir bin) is a tree as a whole:
// every file below it comes from the source; so is a directory operand that a source naming a directory's contents (evil/.,
// cp -r -t bin evil/.) fills. A single operand of ln links into the working directory. An operand the reader cannot evaluate is
// not returned.
func CopiedPaths(e Exec) (files, trees []string) {
	if e.Name != "ln" && e.Name != "cp" && e.Name != "mv" && e.Name != "install" {
		return nil, nil
	}
	var operands []Word
	target := (*Word)(nil)
	noTarget := false
	recursive := e.Name == "mv" || e.Name == "ln"
	parents := false
	for i := 0; i < len(e.Args); i++ {
		a := e.Args[i]
		if !a.Known {
			operands = append(operands, a)
			continue
		}
		v := a.Value
		switch {
		case v == "--":
			operands = append(operands, e.Args[i+1:]...)
			i = len(e.Args)
		case v == "-" || !strings.HasPrefix(v, "-"):
			operands = append(operands, a)
		case strings.HasPrefix(v, "--target-directory="):
			t := Word{Known: true, Value: strings.TrimPrefix(v, "--target-directory=")}
			target = &t
		case v == "--target-directory":
			if i+1 < len(e.Args) {
				i++
				t := e.Args[i]
				target = &t
			}
		case v == "--no-target-directory":
			noTarget = true
		case v == "--recursive" || v == "--archive":
			recursive = true
		case v == "--parents":
			parents = true
		case strings.HasPrefix(v, "--"):
			// a long option: its value, if any, is attached
		default:
			for j := 1; j < len(v); j++ {
				c := v[j]
				if c == 'T' {
					noTarget = true
				}
				if c == 'r' || c == 'R' || c == 'a' {
					recursive = true
				}
				if strings.IndexByte(copyValuedOptions, c) < 0 {
					continue
				}
				if c == 't' {
					if j+1 < len(v) {
						t := Word{Known: true, Value: v[j+1:]}
						target = &t
					} else if i+1 < len(e.Args) {
						i++
						t := e.Args[i]
						target = &t
					}
				} else if j+1 == len(v) {
					i++ // the value is the next word
				}
				break
			}
		}
	}
	below := func(dir Word, sources []Word) {
		for _, s := range sources {
			if !dir.Known || !s.Known {
				continue
			}
			base := path.Base(strings.TrimSuffix(s.Value, "/"))
			if base == "" || base == "." || base == ".." || base == "/" {
				// evil/. (or ., .., /) names a directory's contents, which the copy writes into the directory operand itself.
				trees = append(trees, strings.TrimSuffix(dir.Value, "/"))
				continue
			}
			trees = append(trees, path.Join(dir.Value, base))
			if parents {
				// cp --parents rebuilds the source's own directories below the destination
				trees = append(trees, path.Join(dir.Value, s.Value))
			}
		}
	}
	// whole records that the destination itself may become the copied directory.
	whole := func(dest Word) {
		if recursive && dest.Known && dest.Value != "" {
			trees = append(trees, strings.TrimSuffix(dest.Value, "/"))
		}
	}
	switch {
	case target != nil:
		below(*target, operands)
	case noTarget:
		if n := len(operands); n >= 1 && operands[n-1].Known {
			files = append(files, operands[n-1].Value)
			whole(operands[n-1])
		}
	case len(operands) >= 2:
		last := operands[len(operands)-1]
		if last.Known {
			files = append(files, last.Value)
			whole(last)
			below(last, operands[:len(operands)-1])
		}
	case len(operands) == 1 && e.Name == "ln" && operands[0].Known:
		below(Word{Known: true, Value: "."}, operands)
	}
	return files, trees
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
