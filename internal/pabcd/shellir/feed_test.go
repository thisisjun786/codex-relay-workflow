package shellir

import (
	"reflect"
	"strings"
	"testing"
)

func words(s ...string) []Word {
	out := make([]Word, len(s))
	for i, v := range s {
		out[i] = Word{Known: true, Value: v}
	}
	return out
}

// TestFindScanGuardsAndStarts (CRW-895): the start points and the actions of a find expression, each action with whether a test
// stands before it on every path.
func TestFindScanGuardsAndStarts(t *testing.T) {
	cases := []struct {
		args    string
		starts  string
		actions string // name:guarded, space separated
	}{
		{". -delete", ".", "-delete:false"},
		{". -name x -delete", ".", "-delete:true"},
		{"-delete", "", "-delete:false"},
		{"-L ../repo -type f -delete", "../repo", "-delete:true"},
		{"-D tree .. . -delete", ".. .", "-delete:false"},
		{"-O3 build -delete", "build", "-delete:false"},
		{". -name x -o -delete", ".", "-delete:false"},
		{". -name x -o -name y -delete", ".", "-delete:true"},
		{". ( -name x -o -name y ) -delete", ".", "-delete:true"},
		{". ( -name x -o -true ) -delete", ".", "-delete:false"},
		{". ! -name x -delete", ".", "-delete:true"},
		{". -not -name x -delete", ".", "-delete:true"},
		{". -name x -a -delete", ".", "-delete:true"},
		{". -name -delete", ".", "-delete:true"},
		{". -maxdepth 1 -delete", ".", "-delete:false"},
		{". -print -delete", ".", "-delete:false"},
		{". -exec true {} ; -delete", ".", "-exec:false -delete:false"},
		{". -type d -name build -prune -o -type f -delete", ".", "-delete:true"},
		{". -name x -prune -o -delete", ".", "-delete:false"},
		{". -newermt 2020 -exec rm {} +", ".", "-exec:true"},
		{". -type f -name *.tmp -execdir rm {} +", ".", "-execdir:true"},
		{".. -okdir rm -rf {} ;", "..", "-okdir:false"},
		{". -name x , -delete", ".", "-delete:false"},
		{"-f ../repo -delete", "../repo", "-delete:false"},
		{"-files0-from list -type f -delete", "?", "-delete:true"},
		// the evaluation of 3d1fe314: a test that matches the start point guards nothing
		{"../repo -name * -delete", "../repo", "-delete:false"},
		{"../repo -path * -delete", "../repo", "-delete:false"},
		{"../repo -regex .* -delete", "../repo", "-delete:false"},
		{"../repo -name ???? -delete", "../repo", "-delete:false"},
		{"../repo -name r* -delete", "../repo", "-delete:false"},
		{"../repo -iname REPO -delete", "../repo", "-delete:false"},
		{"../repo -name *.o -delete", "../repo", "-delete:true"},
		{"../repo -name * -type f -delete", "../repo", "-delete:true"},
		{". -name r* -delete", ".", "-delete:true"},
		{"../repo -path */build/* -delete", "../repo", "-delete:true"},
		{"../repo -regex .*\\.o -delete", "../repo", "-delete:true"},
		{"../repo -regextype posix-extended -regex .*\\.(o|a) -delete", "../repo", "-delete:false"},
		{"../repo -regex .*\\(repo\\|x\\) -delete", "../repo", "-delete:false"},
		{"../repo -iregex .*\\(REPO\\|x\\) -delete", "../repo", "-delete:false"},
		{"../repo -name *.o -regex .*\\(repo\\|x\\) -delete", "../repo", "-delete:true"},
		{"../repo -regex .*\\' -delete", "../repo", "-delete:false"},
		{"../repo -regex .*po\\> -delete", "../repo", "-delete:false"},
		{"../repo -regex .*[\\.]po -delete", "../repo", "-delete:false"},
		{"../repo -regex .*/build/.*[0-9] -delete", "../repo", "-delete:true"},
		{"../repo -regex .*[[:digit:]] -delete", "../repo", "-delete:true"},
		{"../repo -regex a^b -delete", "../repo", "-delete:false"},
		{"../repo -regex *repo -delete", "../repo", "-delete:false"},
		{"../repo -regex ^.*$ -delete", "../repo", "-delete:false"},
		{"../repo -regex .+ -delete", "../repo", "-delete:false"},
		// an escaped literal is the character it names (verification of 632401ae)
		{"../repo -regex .*\\* -delete", "../repo", "-delete:true"},
		{"../repo -regex .*\\. -delete", "../repo", "-delete:true"},
		{"../repo -regex .*\\$ -delete", "../repo", "-delete:true"},
		{"../repo -regex .*\\^ -delete", "../repo", "-delete:true"},
		{"../repo -regex .*\\\\ -delete", "../repo", "-delete:true"},
		{".. -regex .*\\. -delete", "..", "-delete:false"},
		{"../repo -regex \\(.*\\) -delete", "../repo", "-delete:false"},
		{"../repo -name x -o -name * -delete", "../repo", "-delete:false"},
		// a group is reached through the tests before it
		{". -type f ( -exec rm {} + )", ".", "-exec:true"},
		{". ( -type f -exec rm {} + )", ".", "-exec:true"},
		{". ( -exec rm {} + )", ".", "-exec:false"},
		{". -type f ( -name x -o -true ) -delete", ".", "-delete:true"},
		{". -type f ( -name x -o -name y -delete )", ".", "-delete:true"},
		{". -name x -o ( -type f -delete )", ".", "-delete:true"},
		{". -name x ( -true -o -true ) -o -delete", ".", "-delete:false"},
		{". ! ( -name x ) -delete", ".", "-delete:false"},
		{". ! -name x -delete", ".", "-delete:true"},
	}
	for _, c := range cases {
		args := words(strings.Fields(c.args)...)
		starts, actions, err := FindScan(args)
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		var gotStarts []string
		for _, s := range starts {
			if !s.Known {
				gotStarts = append(gotStarts, "?")
				continue
			}
			gotStarts = append(gotStarts, s.Value)
		}
		start := "."
		if len(starts) > 0 {
			start = starts[0].Value
		}
		var gotActions []string
		for _, a := range actions {
			gotActions = append(gotActions, a.Name+":"+map[bool]string{true: "true", false: "false"}[a.GuardedFor(start)])
		}
		if strings.Join(gotStarts, " ") != c.starts || strings.Join(gotActions, " ") != c.actions {
			t.Errorf("%q: starts %v actions %v, want %q %q", c.args, gotStarts, gotActions, c.starts, c.actions)
		}
	}
}

func TestFindScanRefusesWhatItCannotRead(t *testing.T) {
	for _, args := range [][]Word{
		words(".", "-exec", "rm", "{}"),
		words(".", "-exec", ";"),
		{{Known: true, Value: "."}, {Reason: "variable"}},
	} {
		if _, _, err := FindScan(args); !isUnreadable(err) {
			t.Errorf("%v: err = %v, want unreadable", args, err)
		}
	}
}

// TestXargsTokensSplitsWhatXargsSplits (CRW-895): the operands xargs builds from its input without -0, -d or -I.
func TestXargsTokensSplitsWhatXargsSplits(t *testing.T) {
	for in, want := range map[string][]string{
		"../repo":               {"../repo"},
		"../repo\n":             {"../repo"},
		"a b\tc\nd":             {"a", "b", "c", "d"},
		`"../repo"`:             {"../repo"},
		"'../repo'\n":           {"../repo"},
		`.\./repo`:              {"../repo"},
		`'a b' c`:               {"a b", "c"},
		`"a b"c`:                {"a bc"},
		"a\x00b":                {"a", "b"},
		`'../repo`:              {"../repo"},
		`a\ b`:                  {"a b"},
		"":                      nil,
		`x\` + "\n" + `../repo`: {"x\n../repo"},
	} {
		if got := xargsTokens(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestEchoAndPrintfOutputs(t *testing.T) {
	cases := []struct {
		cmd  string
		outs []string // the texts; nil with a why is unknown
		why  bool
	}{
		{"echo ../repo", []string{"../repo\n"}, false},
		{"echo -n ../repo", []string{"../repo"}, false},
		{"echo -e '../re\\x70o'", []string{"../repo\n"}, false},
		{"echo -E '../re\\x70o'", []string{"../re\\x70o\n"}, false},
		{"echo '../re\\x70o'", []string{"../re\\x70o\n", "../repo\n"}, false},
		{"echo a b", []string{"a b\n"}, false},
		{"echo", []string{"\n"}, false},
		{"echo -e 'a\\cb'", nil, true},
		{"echo $X", nil, true},
		{"printf '../%s\\n' repo", []string{"../repo\n"}, false},
		{"printf '../re\\160o\\n'", []string{"../repo\n"}, false},
		{"printf '%s/%s\\n' .. repo", []string{"../repo\n"}, false},
		{"printf '%s\\n' a b", []string{"a\nb\n"}, false},
		{"printf '%s' .. /repo", []string{"../repo"}, false},
		{"printf '%b' '../re\\x70o'", []string{"../repo"}, false},
		{"printf '%s %s\\n' a", []string{"a \n"}, false},
		{"printf 'x%%y'", []string{"x%y"}, false},
		{"printf '%d\\n' 7 8", []string{"7\n8\n"}, false},
		{"printf '%c' abc", []string{"a"}, false},
		{"printf 'a\\x41\\101'", []string{"aAA"}, false},
		{"printf -- '%s' a", []string{"a"}, false},
		{"printf '%5s' a", nil, true},
		{"printf '%q' a", nil, true},
		{"printf '%d' 010", nil, true},
		{"printf -v x '%s' a", nil, true},
		{"printf '\\u00e9'", nil, true},
		{"printf", nil, true},
		{"printf '%s' $X", nil, true},
	}
	for _, c := range cases {
		res, err := Analyze(c.cmd, "/w")
		if err != nil {
			t.Fatalf("%q: %v", c.cmd, err)
		}
		e := res.Execs[0]
		var outs []string
		why := ""
		if e.Name == "echo" {
			outs, why = echoOutputs(e.Args)
		} else {
			var out string
			out, why = printfOutput(e.Args)
			if why == "" {
				outs = []string{out}
			}
		}
		if (why != "") != c.why || !reflect.DeepEqual(outs, c.outs) {
			t.Errorf("%q: %q (%s), want %q unknown=%v", c.cmd, outs, why, c.outs, c.why)
		}
	}
}

func TestXargsItemsByMode(t *testing.T) {
	for _, c := range []struct {
		out  string
		sub  bool
		o    xargsOpts
		want []string
	}{
		{"a b\n", false, xargsOpts{}, []string{"a", "b"}},
		{"a:b", false, xargsOpts{Delim: ":", DelimSet: true}, []string{"a", "b"}},
		{"a b\x00c", false, xargsOpts{Null: true}, []string{"a b", "c"}},
		{"a b\n c\n", false, xargsOpts{Replace: "{}"}, []string{"a b", "c"}},
		{"x\\\n../repo", true, xargsOpts{}, []string{"x\n../repo", `x\`, "../repo"}},
		{"'../repo' x\n", false, xargsOpts{}, []string{"../repo", "x"}},
		{"\"a b\"\n", false, xargsOpts{Replace: "{}"}, []string{`"a b"`, "a b"}},
		{"../repo\n", false, xargsOpts{Null: true}, []string{"../repo\n", "../repo"}},
	} {
		items, why := xargsItems(&pipeSource{outs: []string{c.out}, sub: c.sub}, c.o)
		if why != "" || !reflect.DeepEqual(items, c.want) {
			t.Errorf("%q %+v: %q (%s), want %q", c.out, c.o, items, why, c.want)
		}
	}
}

// feedOf analyzes a command and returns the feed of the first program named name.
func feedOf(t *testing.T, cmd, name string) *Feed {
	t.Helper()
	res, err := Analyze(cmd, "/w")
	if err != nil {
		t.Fatalf("%q: %v", cmd, err)
	}
	for _, e := range res.Execs {
		if e.Name == name && e.Ctx.Feed != nil {
			return e.Ctx.Feed
		}
	}
	t.Fatalf("%q: no %s with a feed in %v", cmd, name, names(res))
	return nil
}

// TestXargsFeedItemsWhatStandardInputCarries (CRW-895): the feed of the program xargs runs.
func TestXargsFeedItemsWhatStandardInputCarries(t *testing.T) {
	cases := []struct {
		cmd    string
		items  string // the operands, joined by |; named says the input is named in the text
		named  bool
		unread bool
	}{
		{"echo ../repo | xargs rm", "../repo", true, false},
		{`printf '%s\n' ../repo | xargs rm`, "../repo", true, false},
		{`printf '../%s\n' repo | xargs rm`, "../repo", true, false},
		{`printf '../re\160o\n' | xargs rm`, "../repo", true, false},
		{`echo '.\./repo' | xargs rm`, "../repo", true, false},
		{`echo "$X" | xargs rm`, "", false, true},
		{`echo "$(cat list.txt)" | xargs rm`, "", false, true},
		{"echo ../repo | cat | xargs rm", "../repo", true, false},
		{"echo ../repo | tee x | sort | xargs rm", "../repo", true, false},
		{"echo ../repo | head -n 1 | xargs rm", "../repo", true, false},
		{"echo ../repo | grep -v x | xargs rm", "../repo", true, false},
		{"echo ../repo | nohup xargs rm", "../repo", true, false},
		{"nohup echo ../repo | xargs rm", "../repo", true, false},
		{"/bin/echo ../repo | xargs rm", "../repo", true, false},
		{"xargs rm <<< ../repo", "../repo", true, false},
		{"xargs rm << EOF\n../repo\nEOF", "../repo", true, false},
		{"cat << EOF | xargs rm\n../repo\nEOF", "../repo", true, false},
		{"find ../repo -type f | xargs rm", "../repo", true, false},
		{"find . | xargs rm", ".", true, false},
		{"pwd | xargs rm", "/w", true, false},
		{"ls -d .. | xargs rm", "..", true, false},
		{"ls | xargs rm", "", false, false},
		{"ls build | xargs rm", "", false, false},
		{"git ls-files -z | xargs -0 rm", "", false, false},
		{"cat list.txt | xargs rm", "", false, false},
		{"xargs rm", "", false, false},
		{"xargs rm < /dev/null", "", false, false},
		{"xargs rm < list.txt", "", false, true},
		{"xargs -a list.txt rm", "", false, true},
		{"xargs -0a list.txt rm", "", false, true},
		{"printf '../repoX' | head -c 7 | xargs rm", "", false, true},
		{"echo ../repo | grep -o x | xargs rm", "", false, true},
		{"echo ../repo | sort -z | xargs rm", "", false, true},
		{"echo ../repo | sed s/a/b/ | xargs rm", "", false, true},
		{"echo ../repo | cut -c1-3 | xargs rm", "", false, true},
		{"realpath . | xargs rm", "", false, true},
		{"git rev-parse --show-toplevel | xargs rm", "", false, true},
		{"find -files0-from l | xargs rm", "", false, true},
		{"find . -printf '%p' | xargs rm", "", false, true},
		{"(echo ../repo) | xargs rm", "", false, true},
		{"{ echo ../repo; } | xargs rm", "", false, true},
		{"f() { echo ../repo; }; f | xargs rm", "", false, true},
		{"bash -c 'echo ../repo' | xargs rm", "", false, true},
		{"echo ../repo | xargs echo | xargs rm", "", false, true},
	}
	for _, c := range cases {
		f := feedOf(t, c.cmd, "rm")
		if f.Wrapper != "xargs" || strings.Join(f.Items, "|") != c.items || f.Named != c.named || (f.Unread != "") != c.unread {
			t.Errorf("%q: %+v, want items %q named %v unread %v", c.cmd, f, c.items, c.named, c.unread)
		}
	}
}

// TestXargsReplaceReadsTheTemplateOncePerName: with -I the reader puts each name into the words of the command (the text of a
// shell included), so the consumer judges the commands that run.
func TestXargsReplaceReadsTheTemplateOncePerName(t *testing.T) {
	res, err := Analyze("printf 'a\\nb\\n' | xargs -I{} sh -c 'rm ../{} x'", "/w")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range res.Execs {
		if e.Name == "rm" {
			if e.Ctx.Feed == nil || !e.Ctx.Feed.Replaced || len(e.Ctx.Feed.Items) != 1 {
				t.Fatalf("rm feed %+v", e.Ctx.Feed)
			}
			got = append(got, e.Args[0].Value+" "+e.Ctx.Feed.Items[0])
		}
	}
	if strings.Join(got, "|") != "../a a|../b b" {
		t.Errorf("rm commands %q", got)
	}
	// names the reader cannot read leave the template as it is, with the replace string unresolved
	f := feedOf(t, "ls | xargs -I{} rm ../{}", "rm")
	if f.Replace != "{}" || f.Replaced || f.Named {
		t.Errorf("unread -I feed %+v", f)
	}
	// the program itself comes from the input
	if _, err := Analyze("ls | xargs -I{} {} -rf x", "/w"); !isUnreadable(err) {
		t.Errorf("xargs -I naming the program from an unknown input: %v", err)
	}
	for cmd, opt := range map[string]string{
		"echo a | xargs -i rm ../{}":   "{}",
		"echo a | xargs -iX rm ../X":   "X",
		"echo a | xargs -I @ rm ../@":  "@",
		"echo a | xargs -r -I{} rm {}": "{}",
	} {
		res, err := Analyze(cmd, "/w")
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		for _, e := range res.Execs {
			if e.Name == "rm" && (e.Ctx.Feed == nil || e.Ctx.Feed.Replace != opt || !e.Ctx.Feed.Replaced) {
				t.Errorf("%q: feed %+v, want replace string %q", cmd, e.Ctx.Feed, opt)
			}
		}
	}
}

// TestFindFeedCarriesStartsAndGuard (CRW-895): the feed of each program find runs.
func TestFindFeedCarriesStartsAndGuard(t *testing.T) {
	for cmd, want := range map[string]string{
		"find .. -exec rmdir {} +":                ".. false",
		"find . -type f -name x -exec rmdir {} +": ". true",
		"find a b -name x -execdir rmdir {} +":    "a b true",
		`find . -ok rmdir {} \;`:                  ". false",
		`find ../repo -okdir rmdir {} \;`:         "../repo false",
		"find . -name x -o -exec rmdir {} +":      ". false",
	} {
		f := feedOf(t, cmd, "rmdir")
		var starts []string
		for _, s := range f.Starts {
			starts = append(starts, s.Value)
		}
		got := strings.Join(starts, " ") + " " + map[bool]string{true: "true", false: "false"}[f.GuardedFor(f.Starts[0].Value)]
		if f.Wrapper != "find" || got != want {
			t.Errorf("%q: %q, want %q", cmd, got, want)
		}
	}
}

// TestFeedOuterKeepsTheFindThatRanXargs: a feed inside a feed keeps the outer one.
func TestFeedOuterKeepsTheFindThatRanXargs(t *testing.T) {
	res, err := Analyze(`find .. -exec sh -c 'ls | xargs rmdir' _ {} \;`, "/w")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Execs {
		if e.Name == "rmdir" {
			if e.Ctx.Feed == nil || e.Ctx.Feed.Wrapper != "xargs" || e.Ctx.Feed.Outer == nil || e.Ctx.Feed.Outer.Wrapper != "find" || !e.Ctx.Feed.Outer.Carried || e.Ctx.Feed.Carried {
				t.Fatalf("rmdir feed %+v, want xargs inside a carried find", e.Ctx.Feed)
			}
			return
		}
	}
	t.Fatal("no rmdir")
}

// TestPlainCommandsCarryNoFeed: a program outside find and xargs has no feed.
func TestPlainCommandsCarryNoFeed(t *testing.T) {
	res, err := Analyze("rm a; echo b | cat", "/w")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Execs {
		if e.Ctx.Feed != nil {
			t.Errorf("%s has a feed %+v", e.Name, e.Ctx.Feed)
		}
	}
}

// TestGlobMatchPOSIXClasses: a find pattern bracket that names a POSIX class ends at its own closing bracket, not at the class's.
func TestGlobMatchPOSIXClasses(t *testing.T) {
	for _, c := range []struct {
		pattern, s     string
		matches, known bool
	}{
		{"[[:alpha:]]*", "repo", true, true},
		{"[[:print:]]*", "repo", true, true},
		{"[[:digit:]]*", "repo", false, true},
		{"[[:digit:]r]*", "repo", true, true},
		{"[^[:alpha:]]*", "repo", false, true},
		{"[![:digit:]]*", "repo", true, true},
		{"[[:upper:]][[:lower:]]", "Ab", true, true},
		{"[[:alpha:]", "[x", false, true},
		{"[]a]x", "]x", true, true},
		{"[a-c]*", "b1", true, true},
		{"[[:nosuch:]]*", "repo", false, false},
		{"[[.a.]]*", "a", false, false},
	} {
		m, k := globMatch(c.pattern, c.s, false)
		if m != c.matches || k != c.known {
			t.Errorf("globMatch(%q, %q) = %v, %v; want %v, %v", c.pattern, c.s, m, k, c.matches, c.known)
		}
	}
}

// TestFilterPassesReadsFileOperands: a filter given a file reads the file, so its output is not the lines of standard input.
func TestFilterPassesReadsFileOperands(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"cat", nil, true},
		{"cat", []string{"-"}, true},
		{"cat", []string{"-n"}, true},
		{"cat", []string{"roots.list"}, false},
		{"cat", []string{"--", "roots.list"}, false},
		{"sort", []string{"-r", "roots.list"}, false},
		{"head", []string{"-n", "1"}, true},
		{"head", []string{"-n", "1", "roots.list"}, false},
		{"tail", []string{"-2"}, true},
		{"uniq", []string{"-c"}, true},
		{"uniq", []string{"in", "out"}, false},
		{"tac", []string{"roots.list"}, false},
		{"tee", []string{"copy.txt"}, true},
		{"grep", []string{"old"}, true},
		{"grep", []string{"old", "roots.list"}, false},
		{"grep", []string{"-e", "old"}, true},
		{"grep", []string{"-e", "old", "roots.list"}, false},
		{"grep", []string{"-v", "-f", "pats"}, true},
		{"grep", []string{"-o", "old"}, false},
		{"grep", []string{"-r", "old"}, false},
	} {
		if _, ok := filterPasses(c.name, words(c.args...)); ok != c.ok {
			t.Errorf("filterPasses(%s %v) = %v, want %v", c.name, c.args, ok, c.ok)
		}
	}
}

// TestGlobMatchCaseAndLocale (third verification of f0857f317): -iname folds letters and ranges but tests a class against the name
// as it is, and outside ASCII the locale decides what ? and a bracket take; where the reader cannot settle it the pattern fits.
func TestGlobMatchCaseAndLocale(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		fold       bool
		matches    bool
	}{
		{"[[:upper:]]*", "Repo", true, true},
		{"[[:upper:]]*", "Repo", false, true},
		{"[[:upper:]]*", "repo", false, false},
		{"repo", "Repo", true, true},
		{"[r]epo", "Repo", true, true},
		{"[a-z]*", "Repo", true, true},
		{"[^[:lower:]]*", "Repo", true, true},
		{"[!x]*", "Repo", true, true},
		{"[[:digit:]]*", "Repo", true, false},
		{"*.o", "Repo", true, false},
		{"[[:alpha:]]*", "리포", false, true},
		{"[[:print:]]*", "리포", true, true},
		{"[[:digit:]]*", "리포", false, true},
		{"??????", "리포", false, true},
		{"*.o", "리포", false, false},
		{"리*", "리포", false, true},
		{"[리]*", "repo", false, true},
	} {
		m, k := globMatch(c.pattern, c.s, c.fold)
		if !k || m != c.matches {
			t.Errorf("globMatch(%q, %q, fold %v) = %v, %v; want %v, true", c.pattern, c.s, c.fold, m, k, c.matches)
		}
	}
	for _, c := range []struct {
		pattern, s string
		fold       bool
	}{
		{`\.\./[[:alpha:]]*`, "../리포", false},
		{`\.\./[^a-z]*`, "../Repo", true},
	} {
		if m, k := regexMatch(c.pattern, c.s, c.fold); !m || !k {
			t.Errorf("regexMatch(%q, %q, fold %v) = %v, %v; want true, true", c.pattern, c.s, c.fold, m, k)
		}
	}
	// A pattern the reader cannot evaluate is not a test that leaves the start point out.
	for _, op := range []string{"[[.r.]]*", "[[=r=]]*", "[[:nosuch:]]*"} {
		if (findTest{name: "-name", operand: op}).excludes("../repo") {
			t.Errorf("-name %q leaves ../repo out; the reader cannot evaluate it", op)
		}
	}
}

// TestCarriedTextHoldingTheReplaceString (third verification of f0857f317): a shell text that holds find's {} anywhere carries the
// feed to every program in it, the feed keeps the directory find runs in, and a cd to a word that holds {} leaves the directory unknown.
func TestCarriedTextHoldingTheReplaceString(t *testing.T) {
	res, err := Analyze(`find .. -maxdepth 0 -exec sh -c 'cd {}; rm -rf repo' \;`, "/w/slot/repo")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res.Execs {
		if e.Name != "rm" {
			continue
		}
		found = true
		f := e.Ctx.Feed
		if f == nil || f.Wrapper != "find" || !f.Carried || !f.TextUses || f.Dir.Path != "/w/slot/repo" || !f.Dir.Known {
			t.Errorf("rm feed %+v, want a carried find feed that the text uses, from /w/slot/repo", f)
		}
		if e.Dir.Known {
			t.Errorf("rm runs in %+v after cd {}, want an unknown directory", e.Dir)
		}
	}
	if !found {
		t.Fatal("no rm")
	}
	res, err = Analyze(`find . -name x -exec sh -c 'cd build; rm -f old.o' \;`, "/w")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Execs {
		if e.Name == "rm" && (e.Ctx.Feed == nil || e.Ctx.Feed.TextUses || !e.Dir.Known || e.Dir.Path != "/w/build") {
			t.Errorf("rm %+v feed %+v: a text without {} carries no use of it and keeps its cd", e.Dir, e.Ctx.Feed)
		}
	}
}
