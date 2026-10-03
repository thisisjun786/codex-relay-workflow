package mergeturn

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// MaxMovementSteps is how many merge commits one automatic restatement accounts for. The first-parent
// line of the base branch is read from the tip down to the base a landing recorded; a line that
// has not reached it after this many commits is not confirmed (the manual restatement stays).
const MaxMovementSteps = 32

// movementTimeout bounds the whole reading of a movement, which a forge answers one commit at a time.
const movementTimeout = 90 * time.Second

// Step is one commit on the first-parent line of a branch: its object name, its parents in order
// and the first line of its message.
type Step struct {
	SHA     string
	Parents []string
	Subject string
}

// Movement is how a branch got from one commit to another along its first-parent line, newest
// step first: Steps[0] is To, each next step is the previous step's first parent, and the line
// stops after the step whose first parent is From, or after MaxMovementSteps steps, or at a root.
type Movement struct {
	From       string
	To         string
	Steps      []Step
	Source     string
	Reference  string
	Repository string
}

// MovementReader is the optional second capability of a target reader: how the base branch got
// from one commit to another. A Reader that does not have it leaves a move unconfirmed.
type MovementReader interface {
	Movement(ctx context.Context, repository, base, from, to string) (Movement, error)
}

// readMovement is a reading of how the branch moved, or why there is none. A nil reader and a
// reader without the capability are the same answer.
func readMovement(ctx context.Context, reader Reader, repository, base, from, to string) (Movement, string) {
	movements, ok := reader.(MovementReader)
	if !ok {
		return Movement{}, "this relay's target reader cannot read how the branch moved"
	}
	movement, err := movements.Movement(ctx, repository, base, from, to)
	if err != nil {
		return Movement{}, "reading how the branch moved failed: " + err.Error()
	}
	return movement, ""
}

// moveReading is what a check reads before its transaction about the landing it compares with:
// which landing and recorded base it read, and the movement from that base to the tip, or why
// there is none.
type moveReading struct {
	landing  string
	from     string
	to       string
	movement Movement
	why      string
}

// readMoveSinceLanding reads how the branch got from the latest landing's recorded base to the
// tip. It returns nil when there is no landing or nothing moved, which is the ordinary check.
func (s *Service) readMoveSinceLanding(ctx context.Context, early store.MergeTurnsRow, tip Tip, reader Reader) (*moveReading, error) {
	landing, err := s.latestLanding(ctx, early.TargetKey)
	if err != nil || landing == nil {
		return nil, err
	}
	recorded := landing.Text("observed_base_sha")
	if SameCommit(recorded, tip.SHA) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, movementTimeout)
	defer cancel()
	movement, why := readMovement(ctx, reader, early.Repository, early.BaseRef, strings.ToLower(strings.TrimSpace(recorded)), tip.SHA)
	return &moveReading{landing: landing.Text("turn_id"), from: recorded, to: tip.SHA, movement: movement, why: why}, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// outsideMerges is the steps of a movement when each is a merge commit that no landing on the
// target records, or the reason the move is not confirmed as out-of-lane merges. landed maps the
// landed_sha of every landing on the target to its turn.
func outsideMerges(m Movement, from, to string, landed map[string]string) ([]Step, string) {
	if !SameCommit(m.From, from) || !SameCommit(m.To, to) {
		return nil, "the reading of the branch does not run from the recorded base to the tip it was read for"
	}
	if len(m.Steps) == 0 {
		return nil, "nothing was read from the tip " + pyvalue.StrRepr(shortSHA(to)) + ", so there is no first-parent line to compare with the recorded base"
	}
	want := to
	reached := -1
	for i, step := range m.Steps {
		if !SameCommit(step.SHA, want) {
			return nil, "the first-parent line read from " + pyvalue.StrRepr(shortSHA(to)) + " is broken at " + pyvalue.StrRepr(shortSHA(step.SHA)) + ", which is not the parent of the commit before it"
		}
		if len(step.Parents) == 0 {
			break
		}
		if SameCommit(step.Parents[0], from) {
			reached = i
			break
		}
		want = step.Parents[0]
	}
	if reached < 0 {
		return nil, "the first-parent line of the branch does not lead from " + pyvalue.StrRepr(shortSHA(to)) + " to the recorded base " + pyvalue.StrRepr(shortSHA(from)) + " within " + strconv.Itoa(MaxMovementSteps) + " commits, so the branch was rewritten or moved by more merges than the relay reads"
	}
	steps := m.Steps[:reached+1]
	for _, step := range steps {
		if len(step.Parents) < 2 {
			return nil, "commit " + pyvalue.StrRepr(shortSHA(step.SHA)) + " on the branch has one parent, so it is not a merge commit and is not known to be a merged pull request"
		}
		if turn, ok := landed[strings.ToLower(step.SHA)]; ok {
			return nil, "commit " + pyvalue.StrRepr(shortSHA(step.SHA)) + " is the landing of turn " + pyvalue.StrRepr(turn) + " recorded by this lane, so the lane's own records disagree with the branch"
		}
	}
	return steps, ""
}

// cleanSubject is a commit's first line made safe to store and quote: control characters become
// spaces and the text is cut to 100 characters.
func cleanSubject(subject string) string {
	subject, _, _ = strings.Cut(subject, "\n")
	subject = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, subject)
	if runes := []rune(subject); len(runes) > 100 {
		subject = string(runes[:100])
	}
	return strings.TrimSpace(subject)
}

// staleLandingDetail is the refusal when the last landing's recorded base is not the base the
// candidate states and the relay did not restate it itself: the sentence the refusal always had,
// then what is known of the cause, why nothing was restated, and the command that repairs it.
func staleLandingDetail(row store.MergeTurnsRow, landing store.Row, base, tip, why string) string {
	landed := landing.Text("turn_id")
	recorded := landing.Text("observed_base_sha")
	holder := landing.Text("holder_task_id")
	project := landing.Text("project_key")
	return "the last landing on this target, turn " + pyvalue.StrRepr(landed) + ", recorded base " + pyvalue.StrRepr(recorded) + " and this restates " + pyvalue.StrRepr(base) + "; the base moved under the candidate. If that recorded base is wrong, the landing's holder " + pyvalue.StrRepr(holder) + " or the supervisor above project " + pyvalue.StrRepr(project) + " re-reads it with merge-turn-restate-base --turn " + landed +
		". The base branch " + pyvalue.StrRepr(row.BaseRef) + " of " + pyvalue.StrRepr(row.Repository) + " reads " + pyvalue.StrRepr(tip) + ", not the base that landing recorded: a merge made outside the lane (an out-of-lane merge) moved it, or that recorded base is wrong. " +
		"The relay records the base again itself only when it confirms the move as merge commits that no landing recorded, and it did not: " + why + ". " +
		"Recovery, run by that holder or the supervisor: merge-turn-restate-base --turn " + quote.Shell(landed) + " --actor " + quote.Shell(holder) + " --evidence '<why the base moved>', then check again"
}
