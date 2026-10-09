package hook

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// gateCommandCase is one command and whether the memory gate stops it when no grant stands.
type gateCommandCase struct {
	command string
	stopped bool
}

// crw941PrefixCases spells name (a GNU long option) as every prefix of it that is at least min letters long, with its value
// attached by = and as the next word, around the command template (%s is the option spelling, root the memory root).
func crw941PrefixCases(name string, min int, template, root string) []gateCommandCase {
	var out []gateCommandCase
	for n := min; n <= len(name); n++ {
		prefix := "--" + name[:n]
		out = append(out,
			gateCommandCase{strings.NewReplacer("OPT", prefix+"="+root+"/a").Replace(template), true},
			gateCommandCase{strings.NewReplacer("OPT", prefix+" "+root+"/a").Replace(template), true})
	}
	return out
}

// TestMemoryGateReadsSortOutputAndSedScriptWrites sends the sort and sed shapes of the CRW-941 re-check through
// HandleMemoryWriteGate: with no grant the write is denied, with a grant it passes and spends the grant, and the controls
// pass without one.
func TestMemoryGateReadsSortOutputAndSedScriptWrites(t *testing.T) {
	cwd, root, env := gateScene(t)
	var cases []gateCommandCase
	// every prefix of --output from --o (no other sort option begins with o), with = and with a separate value
	cases = append(cases, crw941PrefixCases("output", 1, "sort OPT x.txt", root)...)
	cases = append(cases, crw941PrefixCases("output", 1, "sort x.txt OPT", root)...)
	cases = append(cases, crw941PrefixCases("output", 1, "sort -n OPT <<'EOF'\nb\na\nEOF", root)...)
	// every prefix of --expression from --e (no other sed option begins with e), the script as a separate word or attached
	for n := 1; n <= len("expression"); n++ {
		p := "--" + "expression"[:n]
		cases = append(cases,
			gateCommandCase{"sed " + p + " 'w " + root + "/a' x.txt", true},
			gateCommandCase{"sed " + p + "='w " + root + "/a' x.txt", true},
			gateCommandCase{"sed -n " + p + " p " + p + " 'W " + root + "/a' <<'EOF'\nabc\nEOF", true})
	}
	cases = append(cases,
		// the value of another option is a word that splits (d1), the first operand is the script under POSIXLY_CORRECT (d2),
		// the backup suffix of -i names a place (d3)
		gateCommandCase{"K='1 -o " + root + "/a'; sort -k $K x.txt", true},
		gateCommandCase{"K='1 -o " + root + "/a'; sort --key $K x.txt", true},
		gateCommandCase{"POSIXLY_CORRECT=1 sed 'w " + root + "/a' --e p x.txt", true},
		gateCommandCase{"POSIXLY_CORRECT=1 sed 'w " + root + "/a' --expression p x.txt", true},
		gateCommandCase{"sed 'w " + root + "/a' -e p x.txt", true},
		gateCommandCase{"sed --in='" + root + "/*' -e p out.txt", true},
		gateCommandCase{"sed -i'" + root + "/*' p out.txt", true},
		gateCommandCase{"sed -ni'" + root + "/*.bak' p out.txt", true},
		gateCommandCase{"sed -i'bak/*' p out.txt", false},
		gateCommandCase{"sed -i.bak p out.txt", false},
		gateCommandCase{"K=1; sort -k $K -o out.txt x.txt", false},
		gateCommandCase{"sed p -e p x.txt", false},
		gateCommandCase{"sort -o " + root + "/a x.txt", true},
		gateCommandCase{"sort -o" + root + "/a x.txt", true},
		gateCommandCase{"sort -ro " + root + "/a x.txt", true},
		gateCommandCase{"sed 'q;w " + root + "/a' x.txt", true},
		gateCommandCase{"sed 'b end;w " + root + "/a;:end' x.txt", true},
		gateCommandCase{"sed --in 's/a/b/' " + root + "/a", true},
		gateCommandCase{"sed --i=.bak 's/a/b/' " + root + "/a", true},
		// controls: the work tree, the temporary root, a read, a pattern, an ambiguous prefix, a value that looks like -o
		gateCommandCase{"sort -o out.txt x.txt", false},
		gateCommandCase{"sort --o=out.txt x.txt", false},
		gateCommandCase{"sort --ou out.txt x.txt", false},
		gateCommandCase{"sort --s=" + root + "/a x.txt", false},
		gateCommandCase{"sort --key -o " + root + "/a", false},
		gateCommandCase{"sort -- --o=" + root + "/a", false},
		gateCommandCase{"sed -n p x.txt", false},
		gateCommandCase{"sed 's/a/b/' x.txt", false},
		gateCommandCase{"sed --expr='s/w/x/' x.txt", false},
		gateCommandCase{"sed --expression 'w out.txt' x.txt", false},
		gateCommandCase{"sed -e 'w /dev/stdout' x.txt", false},
		gateCommandCase{"sed 'r " + root + "/a' x.txt", false},
		gateCommandCase{"sed -n '/a/{p;q}' x.txt", false},
		gateCommandCase{"sed ':a;N;$!ba;s/\\n/ /g' x.txt", false},
	)
	for _, c := range cases {
		out := HandleMemoryWriteGate(gateBash(t, cwd, c.command), env)
		switch {
		case c.stopped && out == "":
			t.Errorf("not denied without a grant: %q", c.command)
			continue
		case !c.stopped && out != "":
			t.Errorf("denied though it writes nothing under the memories root: %q", c.command)
			continue
		}
		if !c.stopped {
			continue
		}
		gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
		if got := HandleMemoryWriteGate(gateBash(t, cwd, c.command), env); got != "" {
			t.Errorf("not allowed with a grant: %q answered %q", c.command, got)
		}
		if state.ReadState(cwd, gateSession).MemoryWriteGrant {
			t.Errorf("the grant was not spent by %q", c.command)
		}
	}
}

// TestShellIRSortDests reads the output file of sort as getopt_long does.
func TestShellIRSortDests(t *testing.T) {
	cwd := "/w"
	none := func(string) (string, bool) { return "", false }
	for _, c := range []struct {
		command string
		want    []string
	}{
		{"sort --o=M/a x", []string{"M/a"}},
		{"sort --o M/a x", []string{"M/a"}},
		{"sort --ou=M/a x", []string{"M/a"}},
		{"sort --ou M/a x", []string{"M/a"}},
		{"sort --out=M/a x", []string{"M/a"}},
		{"sort --outp M/a x", []string{"M/a"}},
		{"sort --outpu M/a x", []string{"M/a"}},
		{"sort --output=M/a x", []string{"M/a"}},
		{"sort -oM/a x", []string{"M/a"}},
		{"sort -o M/a x", []string{"M/a"}},
		{"sort -rnoM/a x", []string{"M/a"}},
		{"sort x --o=M/a", []string{"M/a"}},
		{"sort -k 1,1 --ou M/a x", []string{"M/a"}},
		{"sort -t , --o=M/a x", []string{"M/a"}},
		{"sort -T /tmp --o=M/a x", []string{"M/a"}},
		{"sort --compress-program gzip --o M/a x", []string{"M/a"}},
		{"sort --parallel=2 --buffer-size 1G --o M/a x", []string{"M/a"}},
		{"sort --o=a --output=b x", []string{"a", "b"}},
		{"sort --key -o M/a", nil}, // -o is the key, M/a is an input
		{"sort -k -o M/a", nil},    // the same for the short key option
		{"sort -t -o M/a", nil},    // -o is the field separator
		{"sort -- --o=M/a", nil},   // after -- the word is a file
		{"sort --s=M/a x", nil},    // ambiguous: --sort or --stable
		{"sort --so=M/a x", nil},   // --sort=VALUE
		{"sort --c=M/a x", nil},    // ambiguous: --check or --compress-program
		{"sort --check=quiet --o=M/a x", []string{"M/a"}},
		{"sort --reverse --unique x", nil},
		{"sort -u -n -k2,2 x", nil},
		{"sort x --output", []string{shellIRUnknownDest}},
		{"sort -o", []string{shellIRUnknownDest}},
		// a value the reader cannot evaluate may split into more words, -o among them (K='1 -o M/a')
		{"sort -k $K x", []string{shellIRUnknownDest}},
		{"sort --key $K x", []string{shellIRUnknownDest}},
		{"sort -S $K x", []string{shellIRUnknownDest}},
		{"sort -rt $K x", []string{shellIRUnknownDest}},
		{"sort --buffer-size=$K x", []string{shellIRUnknownDest}},
		{"sort -o M/a -k $K x", []string{"M/a", shellIRUnknownDest}},
		{"sort -- $K", nil}, // after -- every word is a file
	} {
		got, ok := shellIRWriteDests(c.command, cwd, none)
		if !ok || !slices.Equal(got, c.want) {
			t.Errorf("%q writes %q (read %v), want %q", c.command, got, ok, c.want)
		}
	}
}

// TestShellIRSedDestsReadsOptionsAsGetopt: the files a sed command writes through its script, and the files -i rewrites,
// whatever the spelling of the options.
func TestShellIRSedDestsReadsOptionsAsGetopt(t *testing.T) {
	cwd := "/w"
	none := func(string) (string, bool) { return "", false }
	for _, c := range []struct {
		command string
		want    []string
	}{
		{"sed --expression 'w M/a' x", []string{"M/a"}},
		{"sed --expression='w M/a' x", []string{"M/a"}},
		{"sed --expr='w M/a' x", []string{"M/a"}},
		{"sed --e 'w M/a' x", []string{"M/a"}},
		{"sed --exp p --e 'W M/b' x", []string{"M/b"}},
		{"sed -n --expression p x", nil},
		{"sed -l 5 'w M/a' x", []string{"M/a"}},
		{"sed --line-length 5 'w M/a' x", []string{"M/a"}},
		{"sed --line 5 'w M/a' x", []string{"M/a"}},
		{"sed --quiet 'w M/a' x", []string{"M/a"}},
		{"sed 'w M/a' x -n", []string{"M/a"}},
		{"sed -- 'w M/a' x", []string{"M/a"}},
		{"sed 'q;w M/a' x", []string{"M/a"}},
		{"sed -n 'l 5;w M/a' x", []string{"M/a"}},
		{"sed 'b end;w M/a;:end' x", []string{"M/a;:end"}}, // the file name runs to the end of the line
		{"sed -e 'b end;w M/a' -e ':end' x", []string{"M/a"}},
		{"sed -i 's/a/b/' f1 f2", []string{"s/a/b/", "f1", "f2"}},
		{"sed --in-place 's/a/b/' f1", []string{"s/a/b/", "f1"}},
		{"sed --in 's/a/b/' f1", []string{"s/a/b/", "f1"}},
		{"sed --i=.bak -e p f1", []string{"f1"}},
		{"sed --in-pl --expression p f1", []string{"f1"}},
		{"sed -i -e 's/a/b/' f1", []string{"f1"}},
		{"sed -i '' 's/a/b/' f1", []string{"s/a/b/", "f1"}},
		{"sed -n p f1", nil},
		// the backup suffix of -i names a place: * is the input's name, the suffix is a path
		{"sed -i'M/*' p out.txt", []string{"p", "out.txt", "M/out.txt"}},
		{"sed --in='M/*' -e p out.txt", []string{"out.txt", "M/out.txt"}},
		{"sed -ni'M/*.bak' p d/out.txt", []string{"p", "d/out.txt", "M/d/out.txt.bak", "M/out.txt.bak", "d/M/out.txt.bak"}},
		{"sed -i'bak/*' p d/out.txt", []string{"p", "d/out.txt", "bak/d/out.txt", "bak/out.txt", "d/bak/out.txt"}},
		{"sed -i'M/x' -e p out.txt", []string{"out.txt", "out.txtM/x"}},
		{"sed -i.bak -e p out.txt", []string{"out.txt"}},
		{"sed -i'M/*' -i -e p out.txt", []string{"out.txt"}},
		// POSIXLY_CORRECT stops getopt at the first operand, which is then the script that runs: both readings are judged
		{"sed 'w M/a' --e p x", []string{"M/a"}},
		{"sed 'w M/a' -e p x", []string{"M/a"}},
		{"sed 'w M/a' --expression p x", []string{"M/a"}},
		{"sed p -e 'w M/b' x", []string{"M/b"}},
		{"sed -e p x -e 'w M/b'", []string{"M/b"}},
		{"sed -n p x -n", nil},
	} {
		got, ok := shellIRWriteDests(c.command, cwd, none)
		if !ok || !slices.Equal(got, c.want) {
			t.Errorf("%q writes %q (read %v), want %q", c.command, got, ok, c.want)
		}
	}
}

// TestCRW941ShapesThroughTheThreeEntryPoints sends the re-check shapes through HandleMemoryWriteGate, HandleGitHubPostGuard
// and HandleWorktreeGuardPreTool as a hook does: a write under the memories root is a memory attempt only (the other two see no
// post and no removal), a command the reader cannot prove (a sed script file, an ambiguous or unmodelled option) is refused by all
// three, and a control passes all three.
func TestCRW941ShapesThroughTheThreeEntryPoints(t *testing.T) {
	githubPostTempHome(t)
	rig := newDelRig(t)
	cwd, root, env := gateScene(t)
	for _, dir := range []string{cwd, rig.checkout} {
		if err := os.WriteFile(filepath.Join(dir, "real.sed"), []byte("p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		command string
		want    [3]string // memory, github, worktree
	}{
		{"sort --o=M/a x.txt", [3]string{"attempt", "allow", "allow"}},
		{"sort --ou M/a x.txt", [3]string{"attempt", "allow", "allow"}},
		{"sed --expression 'w M/a' x.txt", [3]string{"attempt", "allow", "allow"}},
		{"sed --expr='w M/a' x.txt", [3]string{"attempt", "allow", "allow"}},
		{"sed -e p --e 'W M/a' <<'EOF'\nabc\nEOF", [3]string{"attempt", "allow", "allow"}},
		{"sed --in 's/a/b/' M/a", [3]string{"attempt", "allow", "allow"}},
		{"sed -f s.sed x.txt", [3]string{"attempt", "deny", "deny"}},
		{"sed --fi=s.sed x.txt", [3]string{"attempt", "deny", "deny"}},
		{"sed --f s.sed x.txt", [3]string{"attempt", "deny", "deny"}},
		// the script file exists (see writeScriptFiles): it is still not read, so all three gates refuse
		{"sed -f real.sed x.txt", [3]string{"attempt", "deny", "deny"}},
		{"sed --file real.sed x.txt", [3]string{"attempt", "deny", "deny"}},
		{"sed --file=real.sed x.txt", [3]string{"attempt", "deny", "deny"}},
		{"sed --fi=real.sed x.txt", [3]string{"attempt", "deny", "deny"}},
		{"sed -nf real.sed x.txt", [3]string{"attempt", "deny", "deny"}},
		{"K='1 -o M/a'; sort -k $K x.txt", [3]string{"attempt", "allow", "allow"}},
		{"POSIXLY_CORRECT=1 sed 'w M/a' --e p x.txt", [3]string{"attempt", "allow", "allow"}},
		{"POSIXLY_CORRECT=1 sed 'e gh pr comment 1 -b x' --expression p x.txt", [3]string{"attempt", "deny", "allow"}},
		{"sed --in='M/*' -e p x.txt", [3]string{"attempt", "allow", "allow"}},
		{"sed --s p x.txt", [3]string{"attempt", "deny", "deny"}},
		{"sort --o=out.txt x.txt", [3]string{"none", "allow", "allow"}},
		{"sort -o out.txt x.txt", [3]string{"none", "allow", "allow"}},
		{"sed -n p x.txt", [3]string{"none", "allow", "allow"}},
		{"sed --expression 's/a/b/' x.txt", [3]string{"none", "allow", "allow"}},
	} {
		command := strings.ReplaceAll(c.command, "M/", root+"/")
		var got [3]string
		got[0], got[1], got[2] = "none", "allow", "allow"
		if HandleMemoryWriteGate(gateBash(t, cwd, command), env) != "" {
			got[0] = "attempt"
		}
		if HandleGitHubPostGuard(githubPostShell(t, cwd, command)) != "" {
			got[1] = "deny"
		}
		raw := gatePayload(t, rig.checkout, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}})
		if HandleWorktreeGuardPreTool(raw, rig.env()) != "" {
			got[2] = "deny"
		}
		if got != c.want {
			t.Errorf("%q: memory/github/worktree = %v, want %v", c.command, got, c.want)
		}
	}
}
