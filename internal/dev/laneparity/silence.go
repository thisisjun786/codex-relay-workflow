//go:build dev

package laneparity

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// SilenceReport is the cell that shows the hook switch gates the ported legs (CRW-392, CRW-1082): one
// fixture per ported leg fired with the switch at off (no file) or at cxc, where every ported leg
// must exit 0 with no output and leave no invocation record. The completion Stop is not behind the
// switch and is left out. A leg that answered, failed or recorded something is named, and so is a leg
// no firing was recorded for, since a leg that never ran is not silent.
type SilenceReport struct {
	State  string       `json:"state"` // off or cxc
	Switch SwitchReport `json:"switch"`
	// Fixtures is the number of fixtures fired, Legs the number of ported legs their firings cover
	// and Firings the number of firings of ported legs (receipts) among them.
	Fixtures int `json:"fixtures"`
	Legs     int `json:"legs"`
	Firings  int `json:"firings"`
	// Loud lists the firings that exited non-zero or printed, Recorded the invocation records the
	// fixtures left, Missing the legs a fixture was chosen for that left no receipt.
	Loud     []string `json:"loud,omitempty"`
	Recorded []string `json:"recorded,omitempty"`
	Missing  []string `json:"missing,omitempty"`
	OK       bool     `json:"ok"`
}

// SilenceStates are the switch states the silence cell holds.
var SilenceStates = []string{SwitchOff, SwitchCXC}

// silenceFixtures picks, for every ported leg, the fixture to fire silent: one that matched with the
// switch at crw, preferring one that answers (so a silent run differs from the run at crw), the first
// by id. The completion Stop is not ported and has no fixture here.
func silenceFixtures(base FireReport) (ids []string, legs []string) {
	pick := map[string]FixtureFire{}
	for _, f := range base.Fixtures {
		if !f.Run || !f.OK || f.Leg == CompletionLeg || f.Leg == "" {
			continue
		}
		prior, have := pick[f.Leg]
		switch {
		case !have, prior.Silent && !f.Silent, prior.Silent == f.Silent && f.ID < prior.ID:
			pick[f.Leg] = f
		}
	}
	for leg, f := range pick {
		legs = append(legs, leg)
		ids = append(ids, f.ID)
	}
	sort.Strings(ids)
	sort.Strings(legs)
	return ids, legs
}

// Silence fires one fixture per ported leg with the switch at state and judges the firings silent.
// base is the run at crw that chose the fixtures.
func Silence(o FireOptions, state string, base FireReport) (SilenceReport, error) {
	ids, legs := silenceFixtures(base)
	o.Switch, o.Fault = state, FaultNone
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = regexp.QuoteMeta(id)
	}
	o.Only = regexp.MustCompile(`^(?:` + strings.Join(quoted, "|") + `)$`)
	rep, err := Fire(o)
	if err != nil {
		return SilenceReport{}, err
	}
	out := judgeSilence(state, rep, legs)
	out.Fixtures = len(ids)
	return out, nil
}

// judgeSilence reads the receipts and the records of a run that held the switch at state: every
// firing of a ported leg must have exited 0 with an empty stdout, no invocation record may be left,
// and every leg in legs must have fired at least once.
func judgeSilence(state string, rep FireReport, legs []string) SilenceReport {
	out := SilenceReport{State: state, Switch: rep.Switch}
	silent := StdoutDigest("")
	fired := map[string]bool{}
	for _, r := range rep.Receipts {
		if r.Leg == CompletionLeg {
			continue
		}
		out.Firings++
		fired[r.Leg] = true
		switch {
		case r.Exit != 0:
			out.Loud = append(out.Loud, fmt.Sprintf("%s step %d (%s): exit %d", r.Fixture, r.Step, r.Leg, r.Exit))
		case r.StdoutSHA256 != silent:
			out.Loud = append(out.Loud, fmt.Sprintf("%s step %d (%s): printed", r.Fixture, r.Step, r.Leg))
		}
	}
	out.Legs = len(fired)
	out.Recorded = slices.Clone(rep.Recorded)
	for _, leg := range legs {
		if !fired[leg] {
			out.Missing = append(out.Missing, leg)
		}
	}
	out.OK = len(out.Loud) == 0 && len(out.Recorded) == 0 && len(out.Missing) == 0 && out.Firings > 0
	return out
}

// printSilence is the text of a silence cell.
func printSilence(w io.Writer, s SilenceReport) {
	for _, l := range s.Loud {
		fmt.Fprintf(w, "FAIL switch %s: %s\n", s.State, l)
	}
	for _, r := range s.Recorded {
		fmt.Fprintf(w, "FAIL switch %s: invocation record %s\n", s.State, r)
	}
	for _, m := range s.Missing {
		fmt.Fprintf(w, "FAIL switch %s: no firing of %s was recorded\n", s.State, m)
	}
	fmt.Fprintf(w, "switch %s: %d fixture(s), %d firing(s) of %d ported leg(s): ", s.State, s.Fixtures, s.Firings, s.Legs)
	if s.OK {
		fmt.Fprintln(w, "every one exited 0 in silence and recorded nothing")
	} else {
		fmt.Fprintln(w, "NOT silent")
	}
}
