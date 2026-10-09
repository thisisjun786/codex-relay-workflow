package hook

import (
	"slices"
	"testing"
)

// TestShellSedScriptWrites reads the write commands of sed scripts: w and W name their file to the end of the line, the w
// flag of s names its file the same way, and an address, a label or the text of a, i and c names nothing.
func TestShellSedScriptWrites(t *testing.T) {
	for _, c := range []struct {
		script string
		want   []string
	}{
		{"w mem/a", []string{"mem/a"}},
		{"W mem/b", []string{"mem/b"}},
		{"s/a/b/w mem/c", []string{"mem/c"}},
		{"s|a|b|gw mem/d", []string{"mem/d"}},
		{"/x/I,/y/!W mem/e", []string{"mem/e"}},
		{"1~2w mem/f\np", []string{"mem/f"}},
		{"p;p", nil},
		{"s/a/b/", nil},
		{"y/abc/xyz/", nil},
		{"a text w mem/g", nil},
		{":loop\nb loop", nil},
		{"r mem/h", nil},
		{"s/w/x/\ns/a/b/w mem/i", []string{"mem/i"}},
		// GNU sed opens every w file when it reads the script, so a w after q, in a block or after a branch is a write
		{"q;w mem/j", []string{"mem/j"}},
		{"q5;w mem/k", []string{"mem/k"}},
		{"Q 7\nw mem/l", []string{"mem/l"}},
		{"l 5;w mem/m", []string{"mem/m"}},
		{"l;w mem/n", []string{"mem/n"}},
		{"b end;w mem/o", []string{"mem/o"}},
		{":a;N;$!ba;s/\\n/ /g", nil},
		{":a;w mem/p", []string{"mem/p"}},
		{"t end\nw mem/q\n:end", []string{"mem/q"}},
		{"/a/{p;q}", nil},
		{"/a/{p;w mem/r\n}", []string{"mem/r"}},
		{"1,3!{w mem/s\n}", []string{"mem/s"}},
		{"/a/{p}", nil},
		{"w", []string{shellIRUnknownDest}},
		{"s/a/b", []string{shellIRUnknownDest}},
		{"k", []string{shellIRUnknownDest}},
	} {
		got := shellSedWriteDests(c.script)
		if !slices.Equal(got, c.want) {
			t.Errorf("sed script %q writes %q, want %q", c.script, got, c.want)
		}
	}
}
