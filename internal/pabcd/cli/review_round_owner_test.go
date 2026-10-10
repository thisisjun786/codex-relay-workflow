package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// reviewRoundOwnerSeed is reviewRoundRunSeed with a second session "rt" at A on the same goalplan, unit and epoch, which opened the
// live plan_audit round r1: the round is rt's.
func reviewRoundOwnerSeed(t *testing.T) string {
	t.Helper()
	cwd := reviewRoundRunSeed(t)
	st := state.ReadState(cwd, "rb")
	st.SessionID = "rt"
	reviewRoundRunMust(t, state.WriteState(cwd, st))
	if res := reviewRoundRunDo(t, cwd, "open", "--session", "rt", "--plan-path", reviewRoundRunDoc); res.Code != 0 {
		t.Fatalf("rt could not open its round: %+v", res)
	}
	if owner := reviewRoundRunRound(t, cwd).OwnerSessionID; owner != "rt" {
		t.Fatalf("round owner %q, want rt", owner)
	}
	return cwd
}

func reviewRoundOwnerSetState(t *testing.T, cwd, session string, mutate func(*state.State)) {
	t.Helper()
	st := state.ReadState(cwd, session)
	mutate(&st)
	reviewRoundRunMust(t, state.WriteState(cwd, st))
}

// CRW-1108 d1: review-round open and abort change only a round the invoking session owns, or one whose owner has left it. A live
// round of another session that is still auditing this plan (at A, bound to the goalplan, at the round's epoch), or whose state cannot
// be read, is refused with exit 1 and the goalplan untouched.
func TestReviewRoundOpenAndAbortRefuseARoundAnotherLiveSessionOwns(t *testing.T) {
	for _, c := range []struct {
		name  string
		owner func(t *testing.T, cwd string)
	}{
		{"the owner is still auditing", func(*testing.T, string) {}},
		{"the owner's state cannot be read", func(t *testing.T, cwd string) {
			reviewRoundRunMust(t, os.WriteFile(state.StatePath(cwd, "rt"), []byte("{not json"), 0o600))
		}},
	} {
		for _, argv := range [][]string{{"abort", "--session", "rb"}, {"open", "--session", "rb", "--plan-path", reviewRoundRunDoc}} {
			t.Run(c.name+"/"+argv[0], func(t *testing.T) {
				cwd := reviewRoundOwnerSeed(t)
				c.owner(t, cwd)
				before := reviewRoundRunBytes(t, cwd)
				res := reviewRoundRunDo(t, cwd, argv...)
				if res.Code != 1 || !strings.Contains(res.Output, "review-round "+argv[0]+": round r1 belongs to session rt") || !strings.Contains(res.Output, "Nothing was written.") {
					t.Errorf("got %+v, want the owner refusal", res)
				}
				if reviewRoundRunBytes(t, cwd) != before {
					t.Error("the refusal changed the goalplan")
				}
			})
		}
	}
}

// The takeover route: a round whose owner left it (moved on from A, rebound to another goalplan or epoch, or has no state file), and
// a round with no owner, can be closed or superseded by any session bound to the goalplan; the owner itself always can.
func TestReviewRoundOpenAndAbortTakeOverARoundItsOwnerLeft(t *testing.T) {
	for _, c := range []struct {
		name  string
		owner func(t *testing.T, cwd string)
	}{
		{"the owner moved on from A", func(t *testing.T, cwd string) {
			reviewRoundOwnerSetState(t, cwd, "rt", func(s *state.State) { s.Phase = state.PhaseB })
		}},
		{"the owner is bound to another goalplan", func(t *testing.T, cwd string) {
			reviewRoundOwnerSetState(t, cwd, "rt", func(s *state.State) { s.Slug = "another-plan" })
		}},
		{"the owner is at another epoch", func(t *testing.T, cwd string) {
			reviewRoundOwnerSetState(t, cwd, "rt", func(s *state.State) { s.PlanEpoch = new("e-probe-2") })
		}},
		{"the owner has no state file", func(t *testing.T, cwd string) {
			reviewRoundRunMust(t, os.Remove(state.StatePath(cwd, "rt")))
		}},
		{"the round has no owner", func(t *testing.T, cwd string) {
			plan := reviewRoundRunPlan(t, cwd)
			for i := range plan.ReviewRounds {
				plan.ReviewRounds[i].OwnerSessionID = ""
			}
			reviewRoundRunMust(t, goalplan.WriteGoalplan(cwd, plan))
		}},
	} {
		t.Run(c.name+"/abort", func(t *testing.T) {
			cwd := reviewRoundOwnerSeed(t)
			c.owner(t, cwd)
			if res := reviewRoundRunDo(t, cwd, "abort", "--session", "rb"); res != (ReviewRoundCliResult{Output: "review-round abort: r1 closed as inconclusive"}) {
				t.Errorf("abort: %+v", res)
			}
		})
		t.Run(c.name+"/open", func(t *testing.T) {
			cwd := reviewRoundOwnerSeed(t)
			c.owner(t, cwd)
			if res := reviewRoundRunOpenDoc(t, cwd); res.Code != 0 {
				t.Errorf("open: %+v", res)
			}
			if round := reviewRoundRunRound(t, cwd); round.OwnerSessionID != "rb" || round.RoundID != "r2" {
				t.Errorf("the new round: %+v", round)
			}
		})
	}
	cwd := reviewRoundOwnerSeed(t)
	if res := reviewRoundRunDo(t, cwd, "abort", "--session", "rt"); res.Code != 0 {
		t.Errorf("the owner's own abort: %+v", res)
	}
}
