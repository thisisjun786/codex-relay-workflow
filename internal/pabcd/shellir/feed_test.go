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
			gotStarts = append(gotStarts, s.Value)
		}
		var gotActions []string
		for _, a := range actions {
			gotActions = append(gotActions, a.Name+":"+map[bool]string{true: "true", false: "false"}[a.Guarded])
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

func TestFeedNamesSplitsWhatXargsSplits(t *testing.T) {
	for in, want := range map[string][]string{
		`../repo`:        {"../repo"},
		`../repo\n`:      {"../repo"},
		`../repo\0`:      {"../repo"},
		`a b\tc`:         {"a", "b", "c"},
		`"../repo"`:      {"../repo"},
		"'../repo'\n":    {"../repo"},
		`%s\n`:           {"%s"},
		`a\nb\000c\x41d`: {"a", "b", "c", "d"},
	} {
		if got := FeedNames(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", in, got, want)
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

func nameValues(f *Feed) string {
	var out []string
	for _, n := range f.Names {
		if n.Known {
			out = append(out, n.Value)
		} else {
			out = append(out, "?")
		}
	}
	return strings.Join(out, " ")
}

// TestXargsFeedNamesWhatStandardInputCarries (CRW-895): the feed of the program xargs runs.
func TestXargsFeedNamesWhatStandardInputCarries(t *testing.T) {
	cases := []struct {
		cmd    string
		names  string
		unread bool
	}{
		{"echo ../repo | xargs rm", "../repo", false},
		{`printf '%s\n' ../repo | xargs rm`, `%s\n ../repo`, false},
		{`echo "$X" | xargs rm`, "?", false},
		{"echo ../repo | cat | xargs rm", "../repo", false},
		{"echo ../repo | tee x | sort | xargs rm", "../repo", false},
		{"echo ../repo | nohup xargs rm", "../repo", false},
		{"nohup echo ../repo | xargs rm", "../repo", false},
		{"xargs rm <<< ../repo", "../repo\n", false},
		{"xargs rm << EOF\n../repo\nEOF", "../repo\n", false},
		{"cat << EOF | xargs rm\n../repo\nEOF", "../repo\n", false},
		{"find ../repo -type f | xargs rm", "../repo", false},
		{"ls | xargs rm", "", false},
		{"git ls-files -z | xargs -0 rm", "", false},
		{"cat list.txt | xargs rm", "", false},
		{"xargs rm", "", false},
		{"xargs rm < /dev/null", "", false},
		{"xargs rm < list.txt", "", true},
		{"xargs -a list.txt rm", "", true},
		{"xargs -0a list.txt rm", "", true},
		{"echo ../repo | sed s/a/b/ | xargs rm", "", true},
		{"(echo ../repo) | xargs rm", "", true},
		{"{ echo ../repo; } | xargs rm", "", true},
		{"f() { echo ../repo; }; f | xargs rm", "", true},
		{"bash -c 'echo ../repo' | xargs rm", "", true},
		{"echo ../repo | xargs echo | xargs rm", "", true},
	}
	for _, c := range cases {
		f := feedOf(t, c.cmd, "rm")
		if f.Wrapper != "xargs" || nameValues(f) != c.names || (f.Unread != "") != c.unread {
			t.Errorf("%q: %+v, want names %q unread %v", c.cmd, f, c.names, c.unread)
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
		got := strings.Join(starts, " ") + " " + map[bool]string{true: "true", false: "false"}[f.Guarded]
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
			if e.Ctx.Feed == nil || e.Ctx.Feed.Wrapper != "xargs" || e.Ctx.Feed.Outer == nil || e.Ctx.Feed.Outer.Wrapper != "find" {
				t.Fatalf("rmdir feed %+v, want xargs inside find", e.Ctx.Feed)
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
