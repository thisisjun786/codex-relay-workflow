package hook

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestShellWriteFStringNestingExact pins the nesting limit: 32 levels of f-strings, or of replacement fields in a format
// spec, are read and name their destination; the 33rd level is unreadable through the same reader the gates use.
func TestShellWriteFStringNestingExact(t *testing.T) {
	cases := []struct {
		name    string
		program string
		bad     bool
	}{}
	for _, n := range []int{32, 33} {
		prog := `open("/m/a", "w")`
		for i := 0; i < n; i++ {
			prog = "f'{" + prog + "}'"
		}
		cases = append(cases, struct {
			name    string
			program string
			bad     bool
		}{"f-string levels " + strconv.Itoa(n), prog, n == 33})
	}
	for _, k := range []int{31, 32} {
		prog := "f'" + strings.Repeat("{x:", k) + `{open("/m/a", "w")}` + strings.Repeat("}", k) + "'"
		cases = append(cases, struct {
			name    string
			program string
			bad     bool
		}{"spec field levels " + strconv.Itoa(k+1), prog, k == 32})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			what, bad := shellWriteFStringUnreadableProgram(c.program)
			if bad != c.bad {
				t.Fatalf("unreadable=%v (%q), want %v", bad, what, c.bad)
			}
			if !c.bad && !slices.Contains(shellVerbOpenWrites(c.program), "/m/a") {
				t.Errorf("the readable program does not name /m/a")
			}
			command := "python3 -c \"" + strings.ReplaceAll(c.program, "\"", "'") + "\""
			_, cmdBad := shellIRFStringUnreadable(command)
			if cmdBad != c.bad {
				t.Errorf("command path: unreadable=%v, want %v", cmdBad, c.bad)
			}
		})
	}
}
