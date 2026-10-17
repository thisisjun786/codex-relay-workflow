package reviewadapter

import (
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const (
	readProofMaxLen   = 64 << 10
	readProofMaxDepth = 8
)

func commandReadsCheckout(command, worktree string) bool {
	if strings.TrimSpace(worktree) == "" {
		return false
	}
	base := ""
	if !filepath.IsAbs(worktree) {
		wd, err := os.Getwd()
		if err != nil {
			return false
		}
		base = wd
	}
	root, ok := resolveProofPath(worktree, base)
	if !ok {
		return false
	}
	return checkoutProgramReads(command, root, root, 0, true, proofInput{})
}

func checkoutProgramReads(command, cwd, root string, depth int, succeeded bool, input proofInput) bool {
	if depth > readProofMaxDepth || len(command) > readProofMaxLen || strings.TrimSpace(command) == "" {
		return false
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	st := &proofState{root: root, cwd: cwd, depth: depth}
	return st.stmtsRead(f.Stmts, input, succeeded)
}

type proofInput struct {
	files []string
	taken bool
}

type proofState struct {
	root    string
	cwd     string
	depth   int
	blocked bool
}

func (st *proofState) stmtsRead(stmts []*syntax.Stmt, input proofInput, succeeded bool) bool {
	read := false
	for i, s := range stmts {
		_, r := st.stmtEval(s, input, succeeded && i == len(stmts)-1)
		read = read || r
		if st.blocked {
			return read
		}
	}
	return read
}

func (st *proofState) stmtEval(s *syntax.Stmt, input proofInput, succeeded bool) (ran, read bool) {
	if st.blocked {
		return false, false
	}
	if s.Negated || s.Background || s.Coprocess || s.Disown {
		st.blocked = true
		return false, false
	}
	eff, ok := st.scanRedirs(s.Redirs, input)
	if st.blocked || !ok {
		return false, false
	}
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		return st.callEval(c, eff.files, eff.taken, succeeded)
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.AndStmt:
			lran, lread := st.stmtEval(c.X, eff, succeeded)
			if st.blocked {
				return false, lread
			}
			if lread {
				return true, true
			}
			if !lran {
				st.blocked = true
				return false, false
			}
			return st.stmtEval(c.Y, eff, succeeded)
		case syntax.OrStmt:
			_, lread := st.stmtEval(c.X, eff, false)
			st.blocked = true
			return false, lread
		case syntax.Pipe, syntax.PipeAll:
			x, y := *st, *st
			_, xread := x.stmtEval(c.X, eff, false)
			yran, yread := y.stmtEval(c.Y, proofInput{taken: true}, succeeded)
			return succeeded || yran, xread || yread
		default:
			st.blocked = true
			return false, false
		}
	case *syntax.Subshell:
		inner := *st
		return succeeded, inner.stmtsRead(c.Stmts, eff, succeeded)
	case *syntax.Block:
		return succeeded, st.stmtsRead(c.Stmts, eff, succeeded)
	default:
		st.blocked = true
		return false, false
	}
}

func (st *proofState) scanRedirs(redirs []*syntax.Redirect, input proofInput) (proofInput, bool) {
	out := input
	for _, r := range redirs {
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			if r.N == nil || r.N.Value == "0" {
				out.taken = true
				out.files = nil
			}
		case syntax.RdrIn, syntax.RdrInOut:
			w, ok := literalWord(r.Word)
			if !ok {
				st.blocked = true
				return out, false
			}
			resolved, ok := st.existingPath(w)
			if !ok {
				return out, false
			}
			if r.N == nil || r.N.Value == "0" {
				out.taken = true
				out.files = []string{resolved}
			}
		case syntax.DplIn:
			w, ok := literalWord(r.Word)
			if !ok {
				st.blocked = true
				return out, false
			}
			if fdWord(w) {
				if r.N == nil || r.N.Value == "0" {
					out.taken = true
					out.files = nil
				}
				continue
			}
			resolved, ok := st.existingPath(w)
			if !ok {
				return out, false
			}
			if r.N == nil || r.N.Value == "0" {
				out.taken = true
				out.files = []string{resolved}
			}
		default:
			w, ok := literalWord(r.Word)
			if !ok {
				st.blocked = true
				return out, false
			}
			if r.Op == syntax.DplOut && fdWord(w) {
				continue
			}
			if !st.creatable(w) {
				return out, false
			}
		}
	}
	return out, true
}

func fdWord(w string) bool {
	if w == "-" {
		return true
	}
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return w != ""
}

func proofPath(path, cwd string) (string, bool) {
	if path == "" {
		return "", false
	}
	if filepath.IsAbs(path) {
		return path, true
	}
	if cwd == "" {
		return "", false
	}
	return cwd + string(os.PathSeparator) + path, true
}

func resolveProofPath(path, cwd string) (string, bool) {
	raw, ok := proofPath(path, cwd)
	if !ok {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(raw)
	return resolved, err == nil
}

func (st *proofState) existingPath(path string) (string, bool) {
	return resolveProofPath(path, st.cwd)
}

func (st *proofState) creatable(path string) bool {
	raw, ok := proofPath(path, st.cwd)
	if !ok {
		return false
	}
	parent, _ := filepath.Split(raw)
	if parent == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.IsDir()
}

func (st *proofState) callEval(c *syntax.CallExpr, stdinFiles []string, stdin, succeeded bool) (ran, read bool) {
	if len(c.Assigns) > 0 {
		st.blocked = true
		return false, false
	}
	if len(c.Args) == 0 {
		return false, false
	}
	argv, ok := literalArgs(c.Args)
	if !ok {
		st.blocked = true
		return false, false
	}
	name := argv[0]
	if strings.ContainsRune(name, '/') {
		if !filepath.IsAbs(name) {
			st.blocked = true
			return false, false
		}
		name = filepath.Base(name)
	}
	args := argv[1:]
	switch name {
	case "sh", "bash", "zsh", "dash":
		return succeeded, st.shellScriptReads(args, proofInput{files: stdinFiles, taken: stdin}, succeeded)
	case "cd", "pushd":
		if st.cdEval(args) {
			return true, false
		}
		st.cwd = ""
		st.blocked = true
		return false, false
	case "popd":
		st.cwd = ""
		st.blocked = true
		return false, false
	case "cat", "nl", "head", "tail":
		read := st.fileReaderReads(name, args, stdinFiles)
		return succeeded || read, read
	case "sed":
		read := st.sedReads(args, stdinFiles)
		return succeeded || read, read
	case "rg":
		read := st.rgReads(args, stdinFiles, stdin)
		return succeeded || read, read
	case "git":
		read := st.gitReads(args)
		return succeeded || read, read
	case "true", ":":
		return true, false
	case "echo", "printf", "pwd", "date", "sleep", "ls", "hostname", "uname", "whoami", "false":
		return succeeded, false
	default:
		st.blocked = true
		return false, false
	}
}

func (st *proofState) cdEval(args []string) bool {
	dir := ""
	physical := false
	for _, a := range args {
		switch a {
		case "-L":
			physical = false
			continue
		case "-P":
			physical = true
			continue
		case "--":
			continue
		}
		if dir != "" || a == "-" || strings.HasPrefix(a, "-") {
			return false
		}
		dir = a
	}
	if dir == "" {
		return false
	}
	target := dir
	if !physical {
		target = filepath.Clean(dir)
	}
	resolved, ok := st.existingPath(target)
	if !ok {
		return false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return false
	}
	st.cwd = resolved
	return true
}

func (st *proofState) shellScriptReads(args []string, input proofInput, succeeded bool) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[0] != '-' {
			return false
		}
		cmd := false
		for _, r := range a[1:] {
			switch r {
			case 'c':
				cmd = true
			case 'l', 'i', 'e', 'f':
			default:
				return false
			}
		}
		if !cmd {
			continue
		}
		if i+1 >= len(args) {
			return false
		}
		return checkoutProgramReads(args[i+1], st.cwd, st.root, st.depth+1, succeeded, input)
	}
	return false
}

type argSpec struct {
	shortVal  string
	shortFlag string
	longVal   map[string]bool
	longFlag  map[string]bool
}

func splitArgs(args []string, spec argSpec) (operands []string, used map[string]bool, ok bool) {
	used = map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			return operands, used, true
		}
		if len(a) > 2 && strings.HasPrefix(a, "--") {
			name := a[2:]
			attached := false
			if j := strings.IndexByte(name, '='); j >= 0 {
				name, attached = name[:j], true
			}
			switch {
			case spec.longVal[name]:
				used[name] = true
				if !attached {
					if i+1 >= len(args) {
						return nil, nil, false
					}
					i++
				}
			case spec.longFlag[name] && !attached:
				used[name] = true
			default:
				return nil, nil, false
			}
			continue
		}
		if len(a) > 1 && a[0] == '-' {
			for j := 1; j < len(a); j++ {
				c := a[j]
				if strings.IndexByte(spec.shortVal, c) >= 0 {
					used[string(c)] = true
					if j == len(a)-1 {
						if i+1 >= len(args) {
							return nil, nil, false
						}
						i++
					}
					break
				}
				if strings.IndexByte(spec.shortFlag, c) < 0 {
					return nil, nil, false
				}
				used[string(c)] = true
			}
			continue
		}
		operands = append(operands, a)
	}
	return operands, used, true
}

var (
	catSpec  = argSpec{shortFlag: "AbeEnstTuv", longFlag: flagSet("number", "number-nonblank", "show-all", "show-ends", "show-tabs", "squeeze-blank", "show-nonprinting")}
	headSpec = argSpec{shortVal: "nc", shortFlag: "qvz0123456789", longVal: flagSet("bytes", "lines"), longFlag: flagSet("quiet", "silent", "verbose", "zero-terminated")}
	tailSpec = argSpec{shortVal: "cns", shortFlag: "fFqvz0123456789", longVal: flagSet("bytes", "lines", "pid", "sleep-interval", "max-unchanged-stats"), longFlag: flagSet("follow", "retry", "quiet", "silent", "verbose", "zero-terminated")}
	nlSpec   = argSpec{shortVal: "bdfhilnsvw", shortFlag: "p", longVal: flagSet("body-numbering", "section-delimiter", "footer-numbering", "header-numbering", "line-increment", "join-blank-lines", "number-format", "number-separator", "starting-line-number", "number-width"), longFlag: flagSet("no-renumber")}
	sedSpec  = argSpec{shortVal: "efl", shortFlag: "nrsEuz", longVal: flagSet("expression", "file"), longFlag: flagSet("silent", "quiet", "regexp-extended", "separate", "null-data", "unbuffered", "posix", "sandbox")}
	rgSpec   = argSpec{
		shortVal:  "efgtTABCjmrEM",
		shortFlag: "niIsSUvlqcwFxopzHN.u",
		longVal: flagSet("regexp", "file", "glob", "type", "type-not", "after-context", "before-context", "context",
			"max-count", "threads", "replace", "encoding", "max-columns", "colors", "sort", "sortr", "iglob", "max-depth",
			"max-filesize", "path-separator", "hostname", "field-match-separator", "field-context-separator",
			"context-separator", "type-add", "type-clear", "engine", "dfa-size-limit", "regex-size-limit"),
		longFlag: flagSet("line-number", "no-line-number", "ignore-case", "smart-case", "case-sensitive", "multiline",
			"multiline-dotall", "files-with-matches", "quiet", "count", "count-matches", "invert-match", "word-regexp",
			"line-regexp", "fixed-strings", "only-matching", "pretty", "hidden", "no-hidden", "no-ignore", "no-ignore-vcs",
			"no-ignore-parent", "no-ignore-dot", "no-ignore-exclude", "follow", "stats", "with-filename", "no-filename",
			"heading", "no-heading", "column", "byte-offset", "line-buffered", "null", "passthru", "vimgrep",
			"no-messages", "unrestricted", "text", "binary", "mmap", "no-mmap", "pcre2", "one-file-system", "no-require-git"),
	}
)

func flagSet(names ...string) map[string]bool {
	s := make(map[string]bool, len(names))
	for _, n := range names {
		s[n] = true
	}
	return s
}

var readerSpecs = map[string]argSpec{"cat": catSpec, "nl": nlSpec, "head": headSpec, "tail": tailSpec}

func (st *proofState) readerPathsRead(operands, stdinFiles []string, directory bool) bool {
	readsStdin := len(operands) == 0
	for _, p := range operands {
		if p == "-" {
			readsStdin = true
			continue
		}
		if st.pathReads(p, directory) {
			return true
		}
	}
	if readsStdin {
		for _, p := range stdinFiles {
			if st.pathReads(p, false) {
				return true
			}
		}
	}
	return false
}

func (st *proofState) fileReaderReads(name string, args, stdinFiles []string) bool {
	operands, _, ok := splitArgs(args, readerSpecs[name])
	if !ok {
		return false
	}
	return st.readerPathsRead(operands, stdinFiles, false)
}

func (st *proofState) sedReads(args, stdinFiles []string) bool {
	operands, used, ok := splitArgs(args, sedSpec)
	if !ok {
		return false
	}
	if !used["e"] && !used["expression"] && !used["f"] && !used["file"] {
		if len(operands) == 0 {
			return false
		}
		operands = operands[1:]
	}
	return st.readerPathsRead(operands, stdinFiles, false)
}

func (st *proofState) rgReads(args, stdinFiles []string, stdin bool) bool {
	operands, used, ok := splitArgs(args, rgSpec)
	if !ok {
		return false
	}
	paths := operands
	if !used["e"] && !used["regexp"] && !used["f"] && !used["file"] {
		if len(paths) == 0 {
			return false
		}
		paths = paths[1:]
	}
	if st.readerPathsRead(paths, stdinFiles, true) {
		return true
	}
	return len(paths) == 0 && len(stdinFiles) == 0 && !stdin && insideRoot(st.cwd, st.root)
}

func (st *proofState) gitReads(args []string) bool {
	repo := st.cwd
	i := 0
verb:
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C":
			i++
			if i >= len(args) {
				return false
			}
			dirSt := *st
			dirSt.cwd = repo
			dir, ok := dirSt.existingPath(args[i])
			if !ok {
				return false
			}
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				return false
			}
			repo = dir
		case a == "--no-pager" || a == "--paginate" || a == "-p" || a == "-P":
		case strings.HasPrefix(a, "-"):
			return false
		default:
			break verb
		}
	}
	if i >= len(args) || !insideRoot(repo, st.root) {
		return false
	}
	switch args[i] {
	case "show":
		return st.gitShowReads(args[i+1:], repo)
	case "diff":
		return st.gitDiffReads(args[i+1:], repo)
	default:
		return false
	}
}

var gitPatchFlags = flagSet(
	"-p", "-u", "-w", "-b", "-F", "--patch", "--no-color", "--no-ext-diff", "--full-index", "--abbrev-commit",
	"--no-renames", "--exit-code", "--textconv", "--no-textconv", "--cached", "--staged", "--merge-base", "--no-prefix",
	"--ignore-all-space", "--ignore-space-change", "--ignore-blank-lines", "--ignore-space-at-eol", "--ignore-cr-at-eol",
	"--minimal", "--patience", "--histogram", "--no-indent-heuristic", "--ignore-submodules", "--no-relative",
)

var gitPatchOptionalValOpts = flagSet("--unified", "--color", "--abbrev", "--find-renames", "--find-copies", "--relative")

var gitPatchValOpts = flagSet(
	"--src-prefix", "--dst-prefix", "--line-prefix", "--diff-filter", "--diff-algorithm", "--ws-error-highlight",
)

func (st *proofState) gitPatchOpt(args []string, i *int) bool {
	a := args[*i]
	if strings.HasPrefix(a, "--") {
		name, attached := a, false
		if j := strings.IndexByte(a, '='); j >= 0 {
			name, attached = a[:j], true
		}
		if gitPatchOptionalValOpts[name] {
			return true
		}
		if gitPatchValOpts[name] {
			if !attached {
				*i++
			}
			return *i < len(args)
		}
		return gitPatchFlags[name] && !attached
	}
	if strings.HasPrefix(a, "-U") {
		if len(a) > 2 {
			return true
		}
		*i++
		return *i < len(args)
	}
	return gitPatchFlags[a]
}

func (st *proofState) gitShowReads(args []string, repo string) bool {
	proved, sawObject := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i == len(args)-1 {
				return proved || !sawObject
			}
			for _, p := range args[i+1:] {
				if st.pathspecInside(p, repo) {
					return true
				}
			}
			return false
		case strings.HasPrefix(a, "-"):
			if !st.gitPatchOpt(args, &i) {
				return false
			}
		case strings.Contains(a, ":"):
			_, path, _ := strings.Cut(a, ":")
			if path == "" {
				return false
			}
			sawObject = true
			if st.repoPathInside(path, repo) {
				proved = true
			}
		default:
			sawObject = true
			if resolved, ok := st.pathOperand(a, repo); ok {
				if !insideRoot(resolved, st.root) {
					return false
				}
				proved = true
				continue
			}
			if !looksLikeHex(a) && !strings.Contains(a, "^{tree}") && !strings.Contains(a, "^{blob}") {
				proved = true
			}
		}
	}
	return proved || !sawObject
}

func (st *proofState) gitDiffReads(args []string, repo string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i == len(args)-1 {
				return true
			}
			for _, p := range args[i+1:] {
				if st.pathspecInside(p, repo) {
					return true
				}
			}
			return false
		case strings.HasPrefix(a, "-"):
			if !st.gitPatchOpt(args, &i) {
				return false
			}
		default:
			if resolved, ok := st.pathOperand(a, repo); ok && !insideRoot(resolved, st.root) {
				return false
			}
		}
	}
	return true
}

func (st *proofState) pathOperand(a, base string) (string, bool) {
	if filepath.IsAbs(a) {
		if resolved, err := filepath.EvalSymlinks(a); err == nil {
			return resolved, true
		}
		return filepath.Clean(a), true
	}
	b := *st
	b.cwd = base
	return b.existingPath(a)
}

func (st *proofState) pathspecInside(p, repo string) bool {
	if p == "" || strings.HasPrefix(p, ":") {
		return false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(repo, p)
	}
	return insideRoot(filepath.Clean(p), st.root)
}

func (st *proofState) repoPathInside(p, repo string) bool {
	if p == "" || strings.HasPrefix(p, ":") {
		return false
	}
	base := st.root
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." || p == ".." {
		base = repo
	}
	raw, ok := proofPath(p, base)
	if !ok {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(raw); err == nil {
		info, err := os.Stat(resolved)
		return err == nil && info.Mode().IsRegular() && insideRoot(resolved, st.root)
	}
	return insideRoot(filepath.Clean(raw), st.root)
}

func looksLikeHex(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func (st *proofState) pathReads(path string, directory bool) bool {
	if path == "-" {
		return false
	}
	return checkoutPath(path, st.cwd, st.root, directory)
}

func checkoutPath(path, cwd, root string, directory bool) bool {
	resolved, ok := resolveProofPath(path, cwd)
	if !ok {
		return false
	}
	if !insideRoot(resolved, root) {
		return false
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return directory
	}
	return info.Mode().IsRegular()
}

func insideRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func literalArgs(words []*syntax.Word) ([]string, bool) {
	args := make([]string, 0, len(words))
	for _, w := range words {
		v, ok := literalWord(w)
		if !ok {
			return nil, false
		}
		args = append(args, v)
	}
	return args, true
}

func literalWord(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	var b strings.Builder
	for _, p := range w.Parts {
		if !literalPart(&b, p, false) {
			return "", false
		}
	}
	return b.String(), true
}

func literalPart(b *strings.Builder, p syntax.WordPart, dquote bool) bool {
	switch x := p.(type) {
	case *syntax.Lit:
		unescapeLit(b, x.Value, dquote)
		return true
	case *syntax.SglQuoted:
		if x.Dollar || dquote {
			return false
		}
		b.WriteString(x.Value)
		return true
	case *syntax.DblQuoted:
		if dquote {
			return false
		}
		for _, q := range x.Parts {
			if !literalPart(b, q, true) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func unescapeLit(b *strings.Builder, s string, dquote bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		n := s[i+1]
		if dquote {
			switch n {
			case '"', '\\', '$', '`':
				b.WriteByte(n)
				i++
			case '\n':
				i++
			default:
				b.WriteByte(c)
			}
			continue
		}
		if n != '\n' {
			b.WriteByte(n)
		}
		i++
	}
}
