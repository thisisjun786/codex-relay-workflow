package delivery

import (
	"fmt"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// A second ruling on an event that already has one (CRW-404). docs/relay/README.md, "Ruling an event
// that is already ruled", is the contract; this file is the rule and every word of the route a
// refusal gives, so the text lives in one place.
//
//	recorded        asked           answer
//	any verdict V   V               replay: the recorded record marked _replay, nothing written
//	verified        needs_changes   replace the ruling and open the correction, while nothing rests on it
//	anything else   a different one refuse disposition_conflict, nothing written

// rulingMove is what the transition rule decides.
type rulingMove int

const (
	moveReplay rulingMove = iota
	moveReplace
	moveRefuse
)

// rulingChanged is the reason the journal records beside a ruling that replaced another on an
// unchanged criteria set, as against a re-review (which replaces one because the set moved).
const rulingChanged = "ruling_changed"

// rulingTransition decides what asked does to an event whose recorded ruling is recorded. It is the
// one place the table above lives. For a refusal it returns the sentence that names the route that
// remains; nextGeneration is the generation a recorded needs_changes opened.
func rulingTransition(recorded, asked string, nextGeneration int64) (rulingMove, string) {
	switch {
	case recorded == asked:
		return moveReplay, ""
	case recorded == "verified" && asked == "needs_changes":
		return moveReplace, ""
	case recorded == "verified":
		return moveRefuse, "only needs_changes replaces a verified ruling, because " + asked + " would leave the assignment with no state to act on. Rule needs_changes, or stop the assignment by changing the relationship's status"
	case recorded == "needs_changes":
		return moveRefuse, "the ruling opened generation " + strconv.FormatInt(nextGeneration, 10) + " for the same child and is not withdrawn: rule the head of that generation when the child reports there (assignment-show names it)"
	default:
		return moveRefuse, "a ruling of " + recorded + " is final for this event: to go on, open a fresh execution generation (generation-open, then generation-bind) and rule what the child reports there, as crw-run's 'A fresh execution generation' says"
	}
}

// differentRuling is the refusal of a verdict that cannot replace the recorded one.
func differentRuling(eventID, recorded, turn, asked, route string) error {
	return refuse(DispositionConflict, "%s is already ruled %s (verdict turn %s), and a ruling of %s cannot replace it: %s", strconv.Quote(eventID), recorded, strconv.Quote(turn), asked, route)
}

// builtOn is the refusal of a needs_changes ruling on a verified one that something already acts on.
func builtOn(eventID, turn string, rest registry.Rest) error {
	head := func(sha string) string {
		if len(sha) > 12 {
			return sha[:12]
		}
		return sha
	}
	prefix := fmt.Sprintf("%s is already ruled verified (verdict turn %s), and a ruling of needs_changes cannot replace it: ", strconv.Quote(eventID), strconv.Quote(turn))
	switch rest.Kind {
	case registry.RestsAccepted:
		return refuse(DispositionConflict, "%splan %s accepted it as acceptance %s (node %s), and an accepted result is no longer open to a second ruling. Read the node's stale reading: when its action is correct, dag-correct --prepare prints the instruction and the dispatch request id, the generation is opened by hand and dag-correct binds it; when the action is revalidate or hold, that action's own step applies; when the accepted result is current, a base that moved after the acceptance has its own route, dag-base-refresh: the child merges the base into the branch in a generation opened by hand and changes nothing else, the parent rules that generation verified and records it, and the relay proves from git that its head is the accepted head plus merges of the base (crw-run's 'A base refresh the child made after the acceptance'); for every other current result there is no recorded correction route in this build, so report it on the coordination record and do not open a generation dag-correct will refuse",
			prefix, rest.Plan, rest.ID, rest.Node)
	case registry.RestsMarkedMerged:
		return refuse(DispositionConflict, "%sthe work is marked merged (at %s), and a merged result is corrected by new work, not by a second ruling", prefix, rest.At)
	default:
		if rest.State == "landed" {
			return refuse(DispositionConflict, "%smerge turn %s of this assignment landed %s on the target, and a landed result is corrected by new work, not by a second ruling", prefix, rest.ID, head(rest.Head))
		}
		return refuse(DispositionConflict, "%smerge turn %s of this assignment is %s for candidate head %s, so what it does to the target is not settled. Resolve it first (merge-turn-resolve reads the branch), then rule again", prefix, rest.ID, rest.State, head(rest.Head))
	}
}

// changeRoute is the sentence a refusal of the existing path carries when the event was ruled
// verified and a replacement was asked: the reason stays the existing one, and the parent is told what
// to do about it.
func changeRoute(reason string) string {
	switch reason {
	case StaleGeneration:
		return "only the head of the generation the relationship stands on can be ruled again: assignment-show names that head, so rule it when the child reports there"
	case SupersededRevision:
		return "a newer revision of this generation supersedes this event: assignment-show names the newer revision, so rule that one"
	case RevisionAmbiguous:
		return "there is no single head to rule on. For an assignment outside a plan, open a fresh execution generation (generation-open, then generation-bind) and rule what the child reports there; for a plan node this build records no route (dag-correct and dag-accept record only a generation that a ruling, or an accepted stale result on its correct route, opened), so report it on the coordination record and open no generation"
	case RelationshipNotActive:
		return "bring the relationship back with relationship-resume first (a superseded relationship does not come back: its successor owns the issue), then rule again"
	}
	return ""
}

// changeRefusal re-states a refusal of the existing path for an event already ruled verified.
func changeRefusal(reason, eventID, detail string) error {
	return refuse(reason, "%s is already ruled verified and cannot be re-ruled needs_changes: %s. %s", strconv.Quote(eventID), detail, changeRoute(reason))
}
