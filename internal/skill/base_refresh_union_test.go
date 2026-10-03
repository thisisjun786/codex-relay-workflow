package skill

import (
	"reflect"
	"testing"
)

func TestSplitLinesKeepsLineEnds(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a\n", []string{"a\n"}},
		{"a\nb", []string{"a\n", "b"}},
		{"a\n\nb\n", []string{"a\n", "\n", "b\n"}},
		{"\n", []string{"\n"}},
	} {
		got := splitLines([]byte(c.in))
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitLines(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFirstUnmatched(t *testing.T) {
	for _, c := range []struct {
		sub, seq []string
		want     int
	}{
		{nil, nil, -1},
		{nil, []string{"a"}, -1},
		{[]string{"a"}, nil, 0},
		{[]string{"a", "b"}, []string{"a", "x", "b"}, -1},
		{[]string{"a", "b"}, []string{"b", "a"}, 1},
		{[]string{"a", "a"}, []string{"a"}, 1},
		{[]string{"a", "b", "c"}, []string{"a", "b"}, 2},
	} {
		if got := firstUnmatched(c.sub, c.seq); got != c.want {
			t.Errorf("firstUnmatched(%q, %q) = %d, want %d", c.sub, c.seq, got, c.want)
		}
	}
}

func TestCheckUnionTable(t *testing.T) {
	const base = "a\nb\n"
	for _, c := range []struct {
		name                        string
		base, previous, dev, result string
		code                        string
		stats                       unionStats
	}{
		{"both sides appended and the result keeps both", base, base + "p\n", base + "d\n", base + "p\nd\n", "", unionStats{2, 1, 1, 4}},
		{"the dev side first", base, base + "p\n", base + "d\n", base + "d\np\n", "", unionStats{2, 1, 1, 4}},
		{"an interleaving of longer additions", base, base + "p1\np2\n", base + "d1\nd2\n", base + "p1\nd1\np2\nd2\n", "", unionStats{2, 2, 2, 6}},
		{"no base at all, both sides created the list", "", "p\n", "d\n", "p\nd\n", "", unionStats{0, 1, 1, 2}},
		{"one side added nothing", base, base, base + "d\n", base + "d\n", "", unionStats{2, 0, 1, 3}},
		{"a line both sides added stands twice", base, base + "same\n", base + "same\n", base + "same\nsame\n", "", unionStats{2, 1, 1, 4}},
		{"a line both sides added kept once", base, base + "same\n", base + "same\n", base + "same\n", "union_line_lost", unionStats{}},
		{"the previous side's line lost", base, base + "p\n", base + "d\n", base + "d\n", "union_line_lost", unionStats{}},
		{"the dev side's line lost", base, base + "p\n", base + "d\n", base + "p\n", "union_line_lost", unionStats{}},
		{"a side's order reversed", base, base + "p1\np2\n", base + "d\n", base + "p2\np1\nd\n", "union_line_lost", unionStats{}},
		{"a base line dropped", base, base + "p\n", base + "d\n", "b\np\nd\n", "union_line_lost", unionStats{}},
		{"a foreign line", base, base + "p\n", base + "d\n", base + "p\nd\nx\n", "union_line_added", unionStats{}},
		{"a side's line twice", base, base + "p\n", base + "d\n", base + "p\np\nd\n", "union_line_added", unionStats{}},
		{"a base line twice", base, base + "p\n", base + "d\n", base + "a\np\nd\n", "union_line_added", unionStats{}},
		{"the previous side changed a base line", base, "a\nB\np\n", base + "d\n", "a\nB\np\nd\n", "union_not_additions", unionStats{}},
		{"the dev side removed a base line", base, base + "p\n", "b\nd\n", "b\np\nd\n", "union_not_additions", unionStats{}},
		{"a last line without a newline is its own line", base, base + "p\n", base + "d", base + "p\nd", "", unionStats{2, 1, 1, 4}},
		{"the same line given a newline", base, base + "p\n", base + "d", base + "p\nd\n", "union_line_lost", unionStats{}},
		{"an empty result", base, base + "p\n", base + "d\n", "", "union_line_lost", unionStats{}},
		{"blank lines count", "a\n", "a\n\np\n", "a\n\nd\n", "a\n\np\n\nd\n", "", unionStats{1, 2, 2, 5}},
		{"a blank line both sides added kept once", "a\n", "a\n\np\n", "a\n\nd\n", "a\n\np\nd\n", "union_line_lost", unionStats{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			stats, failure := checkUnion([]byte(c.base), []byte(c.previous), []byte(c.dev), []byte(c.result))
			switch {
			case c.code == "" && failure != nil:
				t.Fatalf("refused: %+v", failure)
			case c.code != "" && (failure == nil || failure.code != c.code):
				t.Fatalf("want %s, got %+v", c.code, failure)
			case c.code == "" && stats != c.stats:
				t.Fatalf("stats %+v, want %+v", stats, c.stats)
			}
		})
	}
}
