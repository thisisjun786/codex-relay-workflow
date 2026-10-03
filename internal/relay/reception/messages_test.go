package reception

import (
	"encoding/json"
	"strings"
	"testing"
)

// The reception check words a value of the wrong shape the way Go does: the kind of the value
// ("a string", "an array", "a number") and a value in double quotes or as its JSON. These texts
// are returned in the answer or the refusal; the ledger stores a message's digest, disposition and
// whether it was applied, never a reason.
func TestReceptionMessagesNameTheWrongShapeAsGoDoes(t *testing.T) {
	type check func() error
	for _, c := range []struct {
		name string
		run  check
		want string
	}{
		{"packet is not an object", func() error { return Check("x") },
			"a packet is an object with an envelope and its typed data, not a string"},
		{"region is not an object", func() error { return Check(O("envelope", "x")) },
			"a packet carries a relay-envelope/1 region under envelope, not a string"},
		{"ladder is not an object", func() error { return CheckProgression([]any{}) },
			"a handover ladder is an object of named states, not an array"},
		{"ladder entry is not an object", func() error { return CheckProgression(O(Progression[0], json.Number("1"))) },
			"each handover state is an object with a state and the record that answered it; " + Progression[0] + " is a number"},
		{"observation is not an object", func() error { _, e := Observation("x"); return e },
			"an observation is an object naming its source, not a string"},
		{"observation field of the wrong kind", func() error {
			_, e := Observation(O("source", "git", "prNumber", json.Number("0")))
			return e
		}, "prNumber in an observation is a positive whole number, not 0"},
	} {
		err := c.run()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to contain %q", c.name, err, c.want)
		}
	}
	gaps := &comparison{}
	gaps.compare("kind", "revision", "3", json.Number("3"), "a reason")
	if len(gaps.gaps) != 1 || !strings.Contains(Get(gaps.gaps[0], "reason").(string), "the record holds a number for revision and the packet a string; values of different shapes") {
		t.Errorf("gap: %v", gaps.gaps)
	}
}
