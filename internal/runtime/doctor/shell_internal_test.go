package doctor

import (
	"strings"
	"testing"
)

// The parsed program splits into words as sh does: quotes removed, expansions as written, a
// redirection or an operator not a word, and a command substitution's own command read too.
func TestShellWordsSplitLikeSh(t *testing.T) {
	for program, want := range map[string]string{
		`python3 -c "a b" "${PLUGIN_ROOT}/x.py"`:                        `python3|-c|a b|${PLUGIN_ROOT}/x.py`,
		`bash '/x/state sh' session`:                                    `bash|/x/state sh|session`,
		`"$HOME/.local/share/crw-runtime/current/bin/crw" hook; exit 0`: `$HOME/.local/share/crw-runtime/current/bin/crw|hook / exit|0`,
		`a\ b "c\"d" 'e'f`:                                              `a b|c"d|ef`,
		`a&&b||c|d 2>&1 >>log # comment`:                                `a / b / c / d`,
		"x \"$(y \"z\")\" `w`":                                          "x|$(y \"z\")|`w` / y|z / w",
	} {
		commands, err := shellWords(program)
		var got []string
		for _, words := range commands {
			got = append(got, strings.Join(words, "|"))
		}
		if strings.Join(got, " / ") != want || err != nil {
			t.Errorf("%s: %s (%v)", program, strings.Join(got, " / "), err)
		}
	}
}

// A word's value makes only the expansions the scan can make (a leading ~ or ~/, $HOME,
// $CODEX_HOME, ${PLUGIN_ROOT}); any other names the expansion that is missing, as written.
func TestShellWordValuesMakeOnlyTheScansExpansions(t *testing.T) {
	w := &shellWalker{expand: Expander{Vars: map[string]string{"HOME": "/h", "CODEX_HOME": "/c", "PLUGIN_ROOT": "/p"}}}
	var got []string
	w.visit = func(word shellWord, role int) bool {
		got = append(got, word.Written+"="+word.Value+"|"+word.Missing)
		return true
	}
	w.unreadable = func(string, string) {}
	w.walk(`x ~/b "$HOME/y" $CODEX_HOME/z ${PLUGIN_ROOT}/w '$HOME' $OTHER ~u/x "$(true)" ${HOME:-x} $'a'`, 0)
	want := []string{"x=x|", "~/b=/h/b|", "$HOME/y=/h/y|", "$CODEX_HOME/z=/c/z|", "${PLUGIN_ROOT}/w=/p/w|", "$HOME=$HOME|", "$OTHER=|$OTHER", "~u/x=|~u", "$(true)=|$(true)", "${HOME:-x}=|${HOME:-x}", "$'a'=|$'a'"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

// walked is every word the reader visits as role:word, and every construct it reports.
func walked(program string) (string, []string) {
	names := map[int]string{roleArgument: "arg", roleCommand: "cmd", roleScript: "script"}
	var visits, unreadable []string
	w := &shellWalker{
		visit: func(word shellWord, role int) bool {
			visits = append(visits, names[role]+":"+word.Written)
			return true
		},
		unreadable: func(value, detail string) { unreadable = append(unreadable, value+": "+detail) },
	}
	w.walk(program, 0)
	return strings.Join(visits, " "), unreadable
}

// Every word in a command position is visited as a command: after ;, &, &&, ||, |, a newline,
// a reserved word or leading assignments, and the command that exec, command, env, nohup, nice,
// timeout, time, eval, trap and a shell's -c string run; a builtin runs nothing, a function is
// read where it is defined, and . FILE or sh FILE reads FILE as a script.
func TestShellWalkerFindsEveryCommandPosition(t *testing.T) {
	for _, c := range []struct{ program, want string }{
		{`cd /x && relay stop`, `arg:/x cmd:relay arg:stop`},
		{`exec relay --x`, `cmd:relay arg:--x`},
		{`exec -a name relay`, `arg:name cmd:relay`},
		{`env A=1 -u B relay`, `cmd:env arg:1 arg:B cmd:relay`},
		{`/usr/bin/env relay`, `cmd:/usr/bin/env cmd:relay`},
		{`true; a || b | c & d`, `cmd:a cmd:b cmd:c cmd:d`},
		{"a\nb", `cmd:a cmd:b`},
		{`nohup nice -n 5 timeout -s KILL 10 relay`, `cmd:nohup cmd:nice arg:5 cmd:timeout arg:KILL arg:10 cmd:relay`},
		{`time -p relay`, `cmd:relay`},
		{`"exec" relay`, `cmd:relay`},
		{`if [ -x /r ]; then relay; else other; fi > /dev/null 2>&1`, `arg:-x arg:/r arg:] cmd:relay cmd:other`},
		{`[[ -f /x && -x /y ]] && relay`, `arg:/x arg:/y cmd:relay`},
		{`while read -r l; do relay "$l"; done < file`, `arg:-r arg:l cmd:relay arg:$l`},
		{`(cd /x && relay) 2>/dev/null`, `arg:/x cmd:relay`},
		{`{ relay; } >log`, `cmd:relay`},
		{`sh -c 'cd /x && relay "$1"' sh arg`, `cmd:sh arg:/x cmd:relay arg:$1 arg:sh arg:arg`},
		{`bash -lc relay`, `cmd:bash cmd:relay`},
		{`bash script.sh a`, `cmd:bash script:script.sh arg:a`},
		{`. /x/lib.sh; source lib2.sh`, `script:/x/lib.sh script:lib2.sh`},
		{`echo "$(relay --version)"`, `cmd:relay arg:--version arg:$(relay --version)`},
		{"A=1 B=`x` relay", "arg:1 cmd:x arg:`x` cmd:relay"},
		{`f() { relay; }; f`, `cmd:relay`},
		{`function g { relay; }; g x`, `cmd:relay arg:x`},
		{`case "$1" in start) relay ;; *) other ;; esac`, `arg:$1 cmd:relay cmd:other`},
		{`for x in a b; do relay "$x"; done`, `arg:a arg:b cmd:relay arg:$x`},
		{`command -v relay || install`, `arg:relay cmd:install`},
		{`eval "relay --x"`, `cmd:relay arg:--x`},
		{`trap 'cleanup' EXIT; relay`, `cmd:cleanup arg:EXIT cmd:relay`},
		{"sh <<EOF\nrelay\nEOF\nafter", `cmd:sh cmd:relay cmd:after`},
		{"cat <<-EOF\n\tnot a command\n\tEOF\nafter", `cmd:cat cmd:after`},
		{"cat <<EOF\n$(relay)\nEOF", `cmd:cat cmd:relay`},
		{"cat <<'EOF'\n$(relay)\nEOF", `cmd:cat`},
		{`bash <<< "relay"`, `cmd:bash cmd:relay`},
		{`exec >/dev/null 2>&1`, ``},
	} {
		got, unreadable := walked(c.program)
		if got != c.want || len(unreadable) != 0 {
			t.Errorf("%q\n got %s %v\nwant %s", c.program, got, unreadable, c.want)
		}
	}
}

// A construct the reader does not model where a command could start is reported, never read
// as nothing: the command words around it are still visited.
func TestShellWalkerReportsWhatItCannotRead(t *testing.T) {
	for _, c := range []struct{ program, want, unread string }{
		{`cat x | sh`, `cmd:cat arg:x cmd:sh`, `sh: a shell that reads its program from its standard input`},
		{`echo relay | env bash -e`, `arg:relay cmd:env cmd:bash`, `bash: a shell that reads its program from its standard input`},
		{`env -S 'relay x'`, `cmd:env arg:relay x`, `-S: an option of env`},
		{`echo 'unterminated`, ``, `cannot parse (1:6: reached EOF without closing quote`},
		{`(( i++ )) && relay`, `cmd:relay`, `an arithmetic command`},
		{nested(maxShellDepth + 1), strings.TrimSpace(strings.Repeat("cmd:sh ", maxShellDepth+1)), `nested more than`},
	} {
		got, unreadable := walked(c.program)
		if got != c.want || len(unreadable) != 1 || !strings.Contains(unreadable[0], c.unread) {
			t.Errorf("%q\n got %s %v\nwant %s and %q", c.program, got, unreadable, c.want, c.unread)
		}
	}
}

// nested is relay run through n shells' -c strings, each quoted for the one outside it.
func nested(n int) string {
	program := "relay"
	for i := 0; i < n; i++ {
		program = "sh -c '" + strings.ReplaceAll(program, "'", `'\''`) + "'"
	}
	return program
}
