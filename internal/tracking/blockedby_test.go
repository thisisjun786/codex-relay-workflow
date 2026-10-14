package tracking

import (
	"reflect"
	"testing"
)

func TestParseBlockedByReadsTheBlockedByLinesOfABody(t *testing.T) {
	r := func(repo string, n int) Ref { return Ref{repo, n} }
	cases := []struct {
		name, body string
		want       []Ref
	}{
		{"one", "text\n\nBlocked by: #35\n\n## more", []Ref{r("o/r", 35)}},
		{"several, once each, in order", "Blocked by: #1, #2 and x/y#3\nblocked by: #2\n", []Ref{r("o/r", 1), r("o/r", 2), r("x/y", 3)}},
		{"own repo spelled out is the same issue", "Blocked by: o/r#4, #4", []Ref{r("o/r", 4)}},
		{"bullet", "- Blocked by: #4", []Ref{r("o/r", 4)}},
		{"bold", "**Blocked by:** #5", []Ref{r("o/r", 5)}},
		{"indented", "   Blocked by: #5", []Ref{r("o/r", 5)}},
		{"crlf", "a\r\nBlocked by: #11\r\nb", []Ref{r("o/r", 11)}},
		{"after a header line", "[층 · 세션] 작성.\n\nBlocked by: #35\n", []Ref{r("o/r", 35)}},
		{"parenthesis", "Blocked by: (#9)", []Ref{r("o/r", 9)}},
		{"no space after the colon", "Blocked by:#10", []Ref{r("o/r", 10)}},
		{"two lines", "Blocked by: #1\ntext #2\nBlocked by: #3", []Ref{r("o/r", 1), r("o/r", 3)}},
		{"inside a code fence", "```\nBlocked by: #6\n```\n", nil},
		{"inside a tilde fence", "~~~\nBlocked by: #6\n~~~\nBlocked by: #7", []Ref{r("o/r", 7)}},
		{"mentioned in prose", "this is not Blocked by: #7", nil},
		{"none", "Blocked by: none", nil},
		{"glued to a word", "Blocked by: abc#8", nil},
		{"zero", "Blocked by: #0", nil},
		{"no line", "see #12 for context", nil},
		{"empty", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseBlockedBy("o/r", c.body); !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseBlockedBy(%q) = %v; want %v", c.body, got, c.want)
			}
		})
	}
}
