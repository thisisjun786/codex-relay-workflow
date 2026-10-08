package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// The reproductions of the reader family are rows in testdata/shellir/rows/*.txt. Each row is one complete command with the
// verdict every command gate must give it. The index testdata/shellir/reproductions.tsv names the source of each row (an
// issue text, a frozen evaluation record, a frozen handoff) and lists the sources' prose items that cannot be commands.
//
// Row file format. A line "=== ID | VERDICT [| deviation=VERDICT]" opens a row. Sections follow, each opened by a marker line:
//
//	--why TEXT         one line; the reason for the verdicts (mandatory)
//	--file PATH        the content lines after it are a file the row needs (PATH relative to the working directory, or
//	                   under {TMP}); a marker --file or --cmd or the next row ends the content
//	--link PATH TARGET a symbolic link
//	--fifo PATH        a named pipe
//	--cmd              the command (the lines after it, or the lines before any marker); the final newline is dropped
//
// VERDICT is a class letter or an explicit memory/github/worktree triple:
//
//	B  none/allow/allow     a command no gate has reason to touch (a control)
//	W  attempt/allow/allow  a write into the protected memory root, named or with a destination the gate cannot name
//	U  attempt/deny/deny    a command whose program the reader cannot prove: every gate refuses it
//	R  none/allow/deny      a removal of the managed checkout the reader reads
//	P  none/deny/allow      a GitHub post the guard refuses
//	Q  attempt/deny/allow   a script file a shell runs that posts to GitHub: the memory gate cannot see into the script (an unknown
//	                        destination), the GitHub guard reads it and refuses
//	T  attempt/allow/allow  a script file a shell runs that does nothing the guards refuse: only the memory gate asks a grant
//	E  attempt/allow/deny   an inline program with a run-time name (exec, eval, ...) that also writes or cannot be read
//
// memory is "attempt" (the memory write gate asks for a grant or refuses) or "none"; github and worktree are "deny" or
// "allow". A deviation is a verdict the reader gives that differs from the one the issue asks for, recorded in
// docs/port-cxc/known-defects/CRW-1028.md; the test asserts the deviation, so it cannot drift unnoticed.
//
// Placeholders in commands and files: {MEMORY} the protected memory root, {CHECKOUT} the managed checkout, {WORK} the working
// directory of the memory and GitHub gates, {CODEXHOME} the parent of the memory root, {TMP} a temporary root the GitHub guard trusts, {NUL}, {DEL}, {CR}, {TAB}.

// reproRow is one row of the reproduction files.
type reproRow struct {
	id, cmd, why    string
	want, deviation [3]string // memory, github, worktree
	hasDeviation    bool
	files           []reproFile
	links           [][2]string
	fifos           []string
	file            string // the row file it was read from
}

type reproFile struct{ path, content string }

var reproClasses = map[string][3]string{
	"B": {"none", "allow", "allow"},
	"W": {"attempt", "allow", "allow"},
	"U": {"attempt", "deny", "deny"},
	"R": {"none", "allow", "deny"},
	"P": {"none", "deny", "allow"},
	"Q": {"attempt", "deny", "allow"},
	"T": {"attempt", "allow", "allow"},
	"E": {"attempt", "allow", "deny"},
}

func parseVerdict(s string) ([3]string, error) {
	s = strings.TrimSpace(s)
	if v, ok := reproClasses[s]; ok {
		return v, nil
	}
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return [3]string{}, fmt.Errorf("verdict %q is neither a class nor memory/github/worktree", s)
	}
	v := [3]string{parts[0], parts[1], parts[2]}
	if (v[0] != "attempt" && v[0] != "none") || (v[1] != "deny" && v[1] != "allow") || (v[2] != "deny" && v[2] != "allow") {
		return [3]string{}, fmt.Errorf("verdict %q has an unknown word", s)
	}
	return v, nil
}

// reproductionRows reads the row files in name order.
func reproductionRows() []reproRow {
	paths, err := filepath.Glob("testdata/shellir/rows/*.txt")
	if err != nil || len(paths) == 0 {
		panic(fmt.Sprintf("no reproduction row files: %v", err))
	}
	sort.Strings(paths)
	var rows []reproRow
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			panic(err)
		}
		rows = append(rows, parseReproRows(filepath.Base(p), string(b))...)
	}
	return rows
}

func parseReproRows(name, text string) []reproRow {
	var rows []reproRow
	var cur *reproRow
	section := "cmd"
	var buf []string
	flush := func() {
		if cur == nil || len(buf) == 0 {
			return
		}
		body := strings.Join(buf, "\n")
		switch section {
		case "cmd":
			cur.cmd = body
		case "file":
			cur.files[len(cur.files)-1].content = body
		}
		buf = nil
	}
	closeRow := func() {
		flush()
		if cur != nil {
			rows = append(rows, *cur)
			cur = nil
		}
	}
	for n, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "=== "):
			closeRow()
			f := strings.Split(strings.TrimPrefix(line, "=== "), "|")
			if len(f) < 2 {
				panic(fmt.Sprintf("%s:%d: row header needs an id and a verdict", name, n+1))
			}
			cur = &reproRow{id: strings.TrimSpace(f[0]), file: name}
			var err error
			if cur.want, err = parseVerdict(f[1]); err != nil {
				panic(fmt.Sprintf("%s:%d: %v", name, n+1, err))
			}
			for _, extra := range f[2:] {
				extra = strings.TrimSpace(extra)
				if !strings.HasPrefix(extra, "deviation=") {
					panic(fmt.Sprintf("%s:%d: unknown header field %q", name, n+1, extra))
				}
				if cur.deviation, err = parseVerdict(strings.TrimPrefix(extra, "deviation=")); err != nil {
					panic(fmt.Sprintf("%s:%d: %v", name, n+1, err))
				}
				cur.hasDeviation = true
			}
			section = "cmd"
		case cur == nil:
			if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
				panic(fmt.Sprintf("%s:%d: text outside a row", name, n+1))
			}
		case strings.HasPrefix(line, "--why "):
			flush()
			cur.why = strings.TrimSpace(strings.TrimPrefix(line, "--why "))
		case strings.HasPrefix(line, "--file "):
			flush()
			section = "file"
			cur.files = append(cur.files, reproFile{path: strings.TrimSpace(strings.TrimPrefix(line, "--file "))})
		case strings.HasPrefix(line, "--link "):
			flush()
			f := strings.Fields(strings.TrimPrefix(line, "--link "))
			if len(f) != 2 {
				panic(fmt.Sprintf("%s:%d: --link needs a path and a target", name, n+1))
			}
			cur.links = append(cur.links, [2]string{f[0], f[1]})
		case strings.HasPrefix(line, "--fifo "):
			flush()
			cur.fifos = append(cur.fifos, strings.TrimSpace(strings.TrimPrefix(line, "--fifo ")))
		case line == "--cmd":
			flush()
			section = "cmd"
		default:
			buf = append(buf, line)
		}
	}
	closeRow()
	for i := range rows {
		rows[i].cmd = strings.TrimRight(rows[i].cmd, "\n")
		for j := range rows[i].files {
			rows[i].files[j].content = strings.TrimRight(rows[i].files[j].content, "\n") + "\n"
		}
	}
	return rows
}

// TestReproductionRowFormat: every row has an id, a command and a reason, and ids are unique.
func TestReproductionRowFormat(t *testing.T) {
	seen := map[string]string{}
	for _, row := range reproductionRows() {
		if row.cmd == "" {
			t.Errorf("%s: no command", row.id)
		}
		if row.why == "" {
			t.Errorf("%s: no --why reason", row.id)
		}
		if prev, dup := seen[row.id]; dup {
			t.Errorf("%s: id repeated (%s and %s)", row.id, prev, row.file)
		}
		seen[row.id] = row.file
		if row.hasDeviation && row.deviation == row.want {
			t.Errorf("%s: the deviation equals the wanted verdict; drop it", row.id)
		}
	}
}

// reproScene writes a row's files and links under the working directory, the managed checkout and the temporary root.
func reproScene(t *testing.T, row reproRow, cwd, checkout, tmp string, fill *strings.Replacer) {
	t.Helper()
	for _, f := range row.files {
		p := fill.Replace(f.path)
		content := fill.Replace(f.content)
		for _, base := range []string{cwd, checkout} {
			dst := p
			if !filepath.IsAbs(p) {
				dst = filepath.Join(base, p)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0o644)
			if strings.HasPrefix(content, "#!") {
				mode = 0o755
			}
			if err := os.WriteFile(dst, []byte(content), mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, l := range row.links {
		p, target := fill.Replace(l[0]), fill.Replace(l[1])
		for _, base := range []string{cwd, checkout} {
			dst := p
			if !filepath.IsAbs(p) {
				dst = filepath.Join(base, p)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(dst)
			if err := os.Symlink(target, dst); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range row.fifos {
		p := fill.Replace(f)
		for _, base := range []string{cwd, checkout} {
			dst := p
			if !filepath.IsAbs(p) {
				dst = filepath.Join(base, p)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(dst, 0o644); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
		}
	}
}

// reproGot judges one command with the three gates and names the verdict in the row's words.
func reproGot(r delRig, cwd, root string, env func(string) (string, bool), cmd string) [3]string {
	var got [3]string
	got[0], got[1], got[2] = "none", "allow", "allow"
	if memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env).Surface != "" {
		got[0] = "attempt"
	}
	if _, denied := githubPostJudgeText(cmd, cwd); denied {
		got[1] = "deny"
	}
	if r.verdict(cmd).Deny {
		got[2] = "deny"
	}
	return got
}

// TestReproductionRows judges every reproduction against the three gates: the memory write gate, the GitHub post guard and
// the worktree deletion guard.
func TestReproductionRows(t *testing.T) {
	deviations := 0
	for _, row := range reproductionRows() {
		row := row
		t.Run(row.id, func(t *testing.T) {
			r := newDelRig(t)
			cwd, root, env := gateScene(t)
			tmp := t.TempDir()
			fill := strings.NewReplacer("{MEMORY}", root, "{CODEXHOME}", filepath.Dir(root), "{MEMORY_NOSLASH}", strings.TrimPrefix(root, "/"), "{PAD1M}", strings.Repeat("#", 1<<20+1), "{CHECKOUT}", r.checkout, "{WORK}", cwd, "{TMP}", tmp,
				"{NUL}", "\x00", "{DEL}", "\x7f", "{CR}", "\r", "{TAB}", "\t")
			reproScene(t, row, cwd, r.checkout, tmp, fill)
			cmd := fill.Replace(row.cmd)
			got := reproGot(r, cwd, root, env, cmd)
			want := row.want
			if row.hasDeviation {
				want = row.deviation
			}
			if got != want {
				t.Errorf("memory/github/worktree = %s, want %s (%s)%s: %q", strings.Join(got[:], "/"), strings.Join(want[:], "/"),
					row.why, map[bool]string{true: " [deviation]", false: ""}[row.hasDeviation], cmd)
			}
		})
		if row.hasDeviation {
			deviations++
		}
	}
	t.Logf("%d rows, %d recorded deviations from the verdict the issue asks for", len(reproductionRows()), deviations)
}
