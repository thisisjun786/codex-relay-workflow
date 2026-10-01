package reception

import (
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Test23JSONDepthBoundaryMatchesPython pins the recursion problem each JSON site reports for a
// document nested 9997 to 9999 deep: the golden began as the problem the Python console named for
// the same document at the same site (the packet, the ledger, the settings, the launch-policy
// declaration and the policy it names).
func Test23JSONDepthBoundaryMatchesPython(t *testing.T) {
	for _, site := range []string{"packet", "ledger", "settings", "declaration", "policy"} {
		for _, depth := range []int{9997, 9998, 9999} {
			t.Run(site+"/"+strconv.Itoa(depth), func(t *testing.T) {
				raw := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
				var got string
				switch site {
				case "packet", "ledger", "declaration":
					got = JSONReaderDepthProblem(raw)
				case "settings":
					got = JSONSettingsDepthProblem(raw)
				case "policy":
					// This file is parsed by rolepolicy before packetPolicy's safety scan.
					got = ""
				}
				golden.Check(t, "problem", []byte(got))
			})
		}
	}
}
