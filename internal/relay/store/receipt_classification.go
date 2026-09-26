package store

import "slices"

type ObservationOutcome string

const (
	ReadyForReview    ObservationOutcome = "ready_for_review"
	Failed            ObservationOutcome = "failed"
	Interrupted       ObservationOutcome = "interrupted"
	BlockedNeedsInput ObservationOutcome = "blocked_needs_input"
	OrdinaryTurnEnd   ObservationOutcome = "ordinary_turn_end"
	InProgress        ObservationOutcome = "in_progress"
	Contradictory     ObservationOutcome = "contradictory"
)

const (
	ProducerChild  = "child"
	ProducerDaemon = "daemon_observation"
)

var receiptOutcomes = []ObservationOutcome{ReadyForReview, Failed, Interrupted, BlockedNeedsInput}

// compatibleTurnStatus is receipts.COMPATIBLE_TURN_STATUS: which observed host status can carry
// which asserted outcome. A positive claim may come from the child's own still-running turn
// (it is then staged); an observed failure never carries one.
func compatibleTurnStatus(outcome ObservationOutcome, status string) bool {
	switch outcome {
	case ReadyForReview, BlockedNeedsInput:
		return status == "completed" || status == "inProgress"
	case Failed:
		return status == "completed" || status == "failed"
	case Interrupted:
		return status == "completed" || status == "interrupted"
	default:
		return false
	}
}

// daemonTurnStatus is receipts.DAEMON_TURN_STATUS: a daemon may only restate what it observed.
func daemonTurnStatus(outcome ObservationOutcome, status string) bool {
	return (outcome == Failed || outcome == Interrupted) && string(outcome) == status
}

type ChildClaim struct {
	Outcome  ObservationOutcome
	Producer string
}

// ClassifyObservation is receipts.classify_observation.
func ClassifyObservation(status string, claim *ChildClaim) (ObservationOutcome, error) {
	if status != "completed" && status != "failed" && status != "interrupted" {
		return InProgress, nil
	}
	if claim != nil {
		if claim.Producer != ProducerChild {
			return "", refuse(ReasonProducerNotPermitted, "only the child produces an asserted outcome")
		}
		if !slices.Contains(receiptOutcomes, claim.Outcome) {
			return "", refuse(ReasonOutcomeInconsistent, "unknown outcome %q", claim.Outcome)
		}
		if !compatibleTurnStatus(claim.Outcome, status) {
			return Contradictory, nil
		}
		return claim.Outcome, nil
	}
	switch status {
	case "failed":
		return Failed, nil
	case "interrupted":
		return Interrupted, nil
	default:
		return OrdinaryTurnEnd, nil
	}
}
