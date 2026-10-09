package hook

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// shellMetaBase is a command one command gate judges: the memory write gate (its destinations) or the GitHub post guard
// (a denial). Paths are relative, so the real-shell run writes only inside its temporary directory.
type shellMetaBase struct {
	cmd  string
	gate string
}

func shellMetaBases() []shellMetaBase {
	return []shellMetaBase{
		{"echo x > m/n.md", "memory"},
		{"tee m/n.md < w/a", "memory"},
		{"cp w/a m/b", "memory"},
		{"mv w/a m/c", "memory"},
		{"sed -i 's/a/b/' m/d", "memory"},
		{"dd if=w/a of=m/e", "memory"},
		{"sort -o m/f w/a", "memory"},
		{"python3 -c \"open('m/g','w').write('x')\"", "memory"},
		{"node -e \"require('fs').writeFileSync('m/h','x')\"", "memory"},
		{"cat <<EOF > m/i\nx\nEOF", "memory"},
		{"echo x >| m/j", "memory"},
		{"echo x &> m/k", "memory"},
		{"rm -rf {CHECKOUT}", "worktree"},
		{"find {CHECKOUT} -delete", "worktree"},
		{"git worktree remove {CHECKOUT}", "worktree"},
		{"gh pr comment 1 --body hi", "github"},
		{"gh api -X POST repos/o/r/issues -f body=x", "github"},
		{"gh issue create --title t --body hi", "github"},
	}
}

// shellMetaSingle quotes a command text as one POSIX single-quoted word.
func shellMetaSingle(c string) string {
	return "'" + strings.ReplaceAll(c, "'", `'\''`) + "'"
}

// shellMetaTransform is one rewrite of a command text that keeps what the shell runs.
type shellMetaTransform struct {
	name  string
	apply func(string) string
}

func shellMetaTransforms() []shellMetaTransform {
	letter := func(c string) bool { return c != "" && c[0] >= 'a' && c[0] <= 'z' }
	return []shellMetaTransform{
		{"paren", func(c string) string { return "( " + c + " )" }},
		{"brace", func(c string) string { return "{ " + c + "; }" }},
		{"if", func(c string) string { return "if true; then " + c + "; fi" }},
		{"for", func(c string) string { return "for _i in 1; do " + c + "; done" }},
		{"while", func(c string) string { return "while :; do " + c + "; break; done" }},
		{"case", func(c string) string { return "case x in x) " + c + " ;; esac" }},
		{"function", func(c string) string { return "fn_m() { " + c + "; }; fn_m" }},
		{"background", func(c string) string { return c + " &\nwait" }},
		{"bang", func(c string) string { return "! " + c }},
		{"assign", func(c string) string { return "METAV=1 " + c }},
		{"redirect-prefix", func(c string) string { return "2>/dev/null " + c }},
		{"amp-redirect-prefix", func(c string) string { return "&>/dev/null " + c }},
		{"env", func(c string) string { return "env " + c }},
		{"command", func(c string) string { return "command " + c }},
		{"exec", func(c string) string { return "exec " + c }},
		{"nohup", func(c string) string { return "nohup " + c }},
		{"time", func(c string) string { return "time " + c }},
		{"quoted-first", func(c string) string {
			if !letter(c) {
				return c
			}
			return "\"" + c[:1] + "\"" + c[1:]
		}},
		{"hex-first", func(c string) string {
			if !letter(c) {
				return c
			}
			return fmt.Sprintf("$'\\x%02x'", c[0]) + c[1:]
		}},
		{"octal-first", func(c string) string {
			if !letter(c) {
				return c
			}
			return fmt.Sprintf("$'\\%03o'", c[0]) + c[1:]
		}},
		{"quoted-word", func(c string) string {
			return shellMetaPerChar(c, func(b byte) string { return "\"" + string(b) + "\"" })
		}},
		{"hex-word", func(c string) string {
			return shellMetaPerChar(c, func(b byte) string { return fmt.Sprintf("$'\\x%02x'", b) })
		}},
		{"octal-word", func(c string) string {
			return shellMetaPerChar(c, func(b byte) string { return fmt.Sprintf("$'\\%03o'", b) })
		}},
		{"continuation", func(c string) string {
			if i := strings.IndexByte(c, ' '); i >= 0 {
				return c[:i] + " \\\n" + c[i+1:]
			}
			return c
		}},
		{"comment-newline", func(c string) string { return c + "\n# metamorphic note" }},
		{"bash-c", func(c string) string { return "bash -c " + shellMetaSingle(c) }},
		{"eval", func(c string) string { return "eval " + shellMetaSingle(c) }},
		{"printf-pipe", func(c string) string { return "printf '%s\\n' " + shellMetaSingle(c) + " | bash" }},
		{"here-document", func(c string) string { return "bash <<'METAEOF'\n" + c + "\nMETAEOF" }},
		{"here-string", func(c string) string { return "bash <<< " + shellMetaSingle(c) }},
		{"source-substitution", func(c string) string { return "source <(printf '%s\\n' " + shellMetaSingle(c) + ")" }},
		{"command-substitution", func(c string) string { return ": $(" + c + ")" }},
		{"cd-prefix", func(c string) string { return "cd . && " + c }},
		{"builtin", func(c string) string { return "builtin " + c }},
		{"command-builtin", func(c string) string { return "command builtin " + c }},
		{"fd-alias", func(c string) string { return "exec 9>&2; " + c }},
		{"fd-dup", func(c string) string { return "{ " + c + "; } 9>&1" }},
	}
}

// shellMetaPerChar rewrites every character of the command's first word through quote, so each letter of the program name is
// in its own quoting form (per-character quoting, ANSI-C hex and octal escapes). A text whose first word is not a plain
// lower-case or digit name (a compound opener, an assignment) is returned as it is.
func shellMetaPerChar(c string, quote func(byte) string) string {
	end := 0
	for end < len(c) && (c[end] >= 'a' && c[end] <= 'z' || c[end] >= '0' && c[end] <= '9') {
		end++
	}
	if end == 0 || end < len(c) && c[end] != ' ' {
		return c
	}
	var sb strings.Builder
	for i := 0; i < end; i++ {
		sb.WriteString(quote(c[i]))
	}
	return sb.String() + c[end:]
}

// shellMetaWrapper is a transform that puts a program word in front of the command it is given, so the command after it
// must be a simple command.
func shellMetaWrapper(name string) bool {
	switch name {
	case "env", "command", "exec", "nohup", "time", "builtin", "command-builtin":
		return true
	}
	return false
}

// shellMetaShape is a transform that adds a keyword, a compound form or a leading assignment. Under a wrapper such a
// form is not the command the wrapper runs (env if, command ! and the like run another program or none), so the pair is
// not a variant of the base.
func shellMetaShape(name string) bool {
	switch name {
	case "paren", "brace", "if", "for", "while", "case", "function", "background", "bang", "assign", "cd-prefix", "time", "command-substitution":
		return true
	}
	return false
}

// shellMetaVariants is the variants of one base: every transform once, 120 pairs of transforms drawn with a fixed seed
// (depth two), and for a base that names m/n.md, the protected path moved through other quoting forms that name the same
// file.
func shellMetaVariants(c string) []string {
	ts := shellMetaTransforms()
	var out []string
	for _, t := range ts {
		out = append(out, t.apply(c))
	}
	rng := rand.New(rand.NewSource(1028))
	for n := 0; n < 120; {
		a, b := ts[rng.Intn(len(ts))], ts[rng.Intn(len(ts))]
		if shellMetaWrapper(b.name) && shellMetaShape(a.name) {
			continue
		}
		out = append(out, b.apply(a.apply(c)))
		n++
	}
	if strings.Contains(c, "m/n.md") {
		for _, rw := range []string{"'m/n.md'", "\"m/n.md\"", "m/'n.md'", "m/$'\\x6e.md'", "m//n.md", "./m/n.md"} {
			out = append(out, strings.Replace(c, "m/n.md", rw, 1))
		}
	}
	return out
}

// shellMetaCovers is whether the destinations a variant names cover the base's: each base destination is named under the
// same cleaned path, or the variant has an unknown destination, which the gate asks a grant for.
func shellMetaCovers(got, want []string) bool {
	if slicesContainsUnknown(got) {
		return true
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if path.Clean(g) == path.Clean(w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestShellMetaVerdictNeverMorePermissive checks that no variant of a denied or judged base gets a looser verdict. A
// memory destination the base names must still be named (or be unknown, which the gate asks a grant for); a GitHub post
// the base denies must still be denied.
func TestShellMetaVerdictNeverMorePermissive(t *testing.T) {
	githubPostTempHome(t)
	start := time.Now()
	cases := 0
	rig := newDelRig(t)
	fill := strings.NewReplacer("{CHECKOUT}", rig.checkout)
	for _, base := range shellMetaBases() {
		switch base.gate {
		case "worktree":
			// A removal of the managed checkout is denied, and no variant of it is allowed (CRW-1028, criterion c2).
			if !rig.verdict(fill.Replace(base.cmd)).Deny {
				t.Fatalf("base %q is not denied by the worktree guard", base.cmd)
			}
			for _, v := range shellMetaVariants(base.cmd) {
				cases++
				if got := rig.verdict(fill.Replace(v)); !got.Deny {
					t.Errorf("variant %q of %q is allowed by the worktree guard", v, base.cmd)
				}
			}
		case "memory":
			want := shellWriteDestsTest(base.cmd)
			if len(want) == 0 || slicesContainsUnknown(want) {
				t.Fatalf("base %q is not judged by the memory gate: %q", base.cmd, want)
			}
			for _, v := range shellMetaVariants(base.cmd) {
				cases++
				if got := shellWriteDestsTest(v); !shellMetaCovers(got, want) {
					t.Errorf("variant %q of %q names %q, want %q or unknown", v, base.cmd, got, want)
				}
			}
		case "github":
			if HandleGitHubPostGuard(githubPostShell(t, t.TempDir(), base.cmd)) == "" {
				t.Fatalf("base %q is not denied by the GitHub post guard", base.cmd)
			}
			for _, v := range shellMetaVariants(base.cmd) {
				cases++
				if HandleGitHubPostGuard(githubPostShell(t, t.TempDir(), v)) == "" {
					t.Errorf("variant %q of %q is allowed by the GitHub post guard", v, base.cmd)
				}
			}
		}
	}
	t.Logf("%d variants in %s", cases, time.Since(start).Round(time.Millisecond))
	if time.Since(start) > 90*time.Second {
		t.Errorf("metamorphic run took %s, want under 90s", time.Since(start))
	}
}

func slicesContainsUnknown(d []string) bool {
	for _, v := range d {
		if v == shellIRUnknownDest {
			return true
		}
	}
	return false
}

// shellMetaTraceLine reads the program word of one trace line: bash prints "+ prog" ("++ prog" nested), zsh prints
// "+file:line> prog".
var (
	shellMetaBashTrace = regexp.MustCompile(`^\++ (\S+)`)
	shellMetaZshTrace  = regexp.MustCompile(`^\+[^ ]*> (\S+)`)
)

// shellMetaIgnored are the words a shell traces that are not programs the reader models: keywords, the structural
// builtins, and the functions the variants define.
var shellMetaIgnored = map[string]bool{
	":": true, "true": true, "false": true, "if": true, "then": true, "else": true, "fi": true, "for": true,
	"do": true, "done": true, "while": true, "case": true, "esac": true, "break": true, "wait": true, "{": true,
	"}": true, "(": true, ")": true, "!": true, "cd": true, "fn_m": true, "f": true, "[[": true, "[": true,
}

// shellMetaTrace runs one command under the shell with its trace on, in dir, with recording stubs first on PATH and
// only the system directories after them, and returns the program words the shell traced.
func shellMetaTrace(t *testing.T, shell, cmd, dir, stubs string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"-x", "-c", cmd}
	if path.Base(shell) == "zsh" {
		args = []string{"-f", "-x", "-c", cmd}
	}
	c := exec.CommandContext(ctx, shell, args...)
	c.Dir = dir
	c.Env = []string{
		"PATH=" + stubs + ":/usr/bin:/bin", "HOME=" + filepath.Join(dir, "home"),
		"CODEX_HOME=" + filepath.Join(dir, "codex-home"), "CRW_HOME=" + filepath.Join(dir, "crw-home"),
		"TMPDIR=" + filepath.Join(dir, "tmp"), "LC_ALL=C",
	}
	var stderr bytes.Buffer
	c.Stderr = &stderr
	_ = c.Run()
	re := shellMetaBashTrace
	if path.Base(shell) == "zsh" {
		re = shellMetaZshTrace
	}
	var out []string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// shellMetaReaderNames is the program names the reader lists for a command, and whether it lists a program it cannot
// name. A command the reader refuses lists nothing: the refusal is the verdict.
func shellMetaReaderNames(cmd string) (names map[string]bool, unknown bool, refused bool) {
	r, err := shellir.AnalyzeEnv(cmd, "/work", nil)
	if err != nil {
		return nil, false, true
	}
	names = map[string]bool{}
	for _, e := range r.Execs {
		if !e.Program.Known {
			unknown = true
		}
		if e.Program.Known {
			names[path.Base(e.Program.Value)] = true
		}
		if e.Name != "" {
			names[path.Base(e.Name)] = true
		}
	}
	return names, unknown, false
}

// TestShellMetaRealShellDifferential runs every variant under bash -x and zsh -x in a temporary directory with recording
// stubs. Every program word the shell traced must be a program the reader lists, or the reader must list a program it
// cannot name. The reader may refuse more than the shell runs, never less.
func TestShellMetaRealShellDifferential(t *testing.T) {
	if testing.Short() {
		t.Skip("real-shell differential skipped in -short")
	}
	var shells []string
	for _, s := range []string{"bash", "zsh"} {
		if p, err := exec.LookPath(s); err == nil {
			shells = append(shells, p)
		} else {
			t.Logf("%s is not installed: that shell is skipped", s)
		}
	}
	if len(shells) == 0 {
		t.Skip("no bash or zsh on this host")
	}
	dir := t.TempDir()
	githubPostTempHome(t)
	for _, d := range []string{"m", "w", "home", "codex-home", "crw-home", "tmp", "stubs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "w", "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubs := filepath.Join(dir, "stubs")
	for _, s := range []string{"gh", "curl", "git", "wget"} {
		if err := os.WriteFile(filepath.Join(stubs, s), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var checked, refused, unknown int
	for _, base := range shellMetaBases() {
		for _, v := range append([]string{base.cmd}, shellMetaVariants(base.cmd)...) {
			names, unk, ref := shellMetaReaderNames(v)
			if ref {
				refused++
				continue
			}
			if unk {
				unknown++
				continue
			}
			for _, shell := range shells {
				for _, word := range shellMetaTrace(t, shell, v, dir, stubs) {
					name := path.Base(word)
					if shellMetaIgnored[name] || strings.Contains(name, "=") {
						continue
					}
					checked++
					if !names[name] {
						t.Errorf("%s ran %q, which the reader does not list for %q (listed: %v)", path.Base(shell), name, v, names)
					}
				}
			}
		}
	}
	t.Logf("traced program words checked: %d; commands the reader refused: %d; commands with an unnamed program: %d", checked, refused, unknown)
}
