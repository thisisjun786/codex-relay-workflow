package dagsched

import (
	"strings"
	"testing"
)

// The conflict marker scan reads a file of any size and any line length to the end: a start line and an end line of seven or more marker characters make a file unresolved, and nothing short of that does.
func TestScanConflictMarkers(t *testing.T) {
	long := strings.Repeat("<", 70000)
	cases := []struct {
		name           string
		body           string
		started, ended bool
	}{
		{"git's own markers", "a\n<<<<<<< 1234abc\nours\n=======\ntheirs\n>>>>>>> 5678def\nz\n", true, true},
		{"a marker without a label at the end of the file", "<<<<<<<\n>>>>>>>", true, true},
		{"a wider marker", "<<<<<<<<<<< a\n>>>>>>>>>>> b\n", true, true},
		{"markers with a carriage return", "<<<<<<< a\r\n=======\r\n>>>>>>>\r\n", true, true},
		{"only the start", "<<<<<<< a\nresolved\n", true, false},
		{"six characters are not a marker", "<<<<<<\n>>>>>>\n", false, false},
		{"seven characters and a word are not a marker", "<<<<<<<x\n>>>>>>>y\n", false, false},
		{"markers that do not begin the line", "a <<<<<<< b\n  >>>>>>> c\n", false, false},
		{"a long run followed by text is not a marker", long + "literal\n" + strings.ReplaceAll(long, "<", ">") + "literal\n", false, false},
		{"a long run followed by a space is a marker", long + " a\n" + strings.ReplaceAll(long, "<", ">") + " b\n", true, true},
		{"a long run that ends the line is a marker", long + "\n" + strings.ReplaceAll(long, "<", ">") + "\n", true, true},
		{"markers after a large file", strings.Repeat("line of a large file\n", 500000) + "<<<<<<< a\n=======\n>>>>>>> b\n", true, true},
	}
	for _, c := range cases {
		if started, ended := scanConflictMarkers(strings.NewReader(c.body)); started != c.started || ended != c.ended {
			t.Errorf("%s: started=%v ended=%v, want %v %v", c.name, started, ended, c.started, c.ended)
		}
	}
}
