package manage

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// The plan-summary notice (CRW-1204): the run tells the orchestrator thread that a summary entry is owed with
// `crw manage send-parent --logical-id summary-<summary id>`. The notice's logical id names the summary entry, so it is delivered once
// however often the run asks for it: the send-parent --queue path of such an id writes the notice only when the queue holds none of it
// (pending, in a batch the pump has pinned, or in sent/), and the pump leaves a copy that sent/ already holds out of its batches and
// leaves a settlement for each summary notice it flushed. The notices of every other logical id take the path they always did.
const (
	// summaryLogicalPrefix starts the logical id of a summary notice.
	summaryLogicalPrefix = "summary-"
	// summarySettledDir, in the state directory, holds the settlement of each summary notice the pump flushed.
	summarySettledDir = "summary-settled"

	summaryResultAccepted = "accepted"
	summaryResultRefused  = "refused"

	// The states of a summary notice in the parent queue.
	summaryStatePending  = "pending"
	summaryStateInFlight = "in_flight"
	summaryStateSent     = "sent"
	summaryStateRefused  = "refused"
)

// summarySettlement is what the pump left for a flushed summary notice.
type summarySettlement struct {
	SummaryID string `json:"summary_id"`
	LogicalID string `json:"logical_id"`
	Thread    string `json:"thread"`
	Result    string `json:"result"`
	RequestID string `json:"request_id"`
	At        string `json:"at"`
}

func summaryIsLogical(logicalID string) bool {
	return strings.HasPrefix(logicalID, summaryLogicalPrefix)
}

func summarySettledPath(cfg *Config, thread, logicalID string) string {
	return crwconfig.JoinRoot(cfg.StateDir, summarySettledDir, thread, logicalID+".json")
}

// summaryReadSettlement reads the settlement of a summary notice, ok false when the pump left none.
func summaryReadSettlement(cfg *Config, thread, logicalID string) (summarySettlement, bool) {
	raw, err := os.ReadFile(summarySettledPath(cfg, thread, logicalID))
	if err != nil {
		return summarySettlement{}, false
	}
	var s summarySettlement
	if json.Unmarshal(raw, &s) != nil || s.Result == "" {
		return summarySettlement{}, false
	}
	return s, true
}

// pumpSummarySettle records what the pump did with a summary notice it moved out of the queue; a notice of any other logical id leaves
// nothing.
func pumpSummarySettle(cfg *Config, thread, name, result, requestID string) {
	logicalID := strings.TrimSuffix(name, ".txt")
	if !summaryIsLogical(logicalID) {
		return
	}
	body, err := json.Marshal(summarySettlement{SummaryID: strings.TrimPrefix(logicalID, summaryLogicalPrefix), LogicalID: logicalID, Thread: thread, Result: result, RequestID: requestID,
		At: deliverNow(nil)})
	if err != nil {
		return
	}
	if err := deliverWriteAtomic(summarySettledPath(cfg, thread, logicalID), append(body, 0x0a)); err != nil {
		pumpLog(cfg, fmt.Sprintf("queue %s: the settlement of %s could not be written: %v", thread, logicalID, err))
	}
}

// summaryQueueState says where the summary notice of a logical id is in the parent queue of a thread: "" when the queue holds none of
// it. A settlement the pump left counts as the notice's end whatever else is on disk.
func summaryQueueState(cfg *Config, thread, logicalID string) (state, requestID string, err error) {
	if s, ok := summaryReadSettlement(cfg, thread, logicalID); ok {
		if s.Result == summaryResultRefused {
			return summaryStateRefused, s.RequestID, nil
		}
		return summaryStateSent, s.RequestID, nil
	}
	dir := crwconfig.JoinRoot(cfg.StateDir, sendParentQueueDir, thread)
	name := logicalID + ".txt"
	for _, probe := range []struct{ dir, state string }{{crwconfig.JoinRoot(dir, pumpSentDir), summaryStateSent}, {crwconfig.JoinRoot(dir, pumpQueueRefusedDir), summaryStateRefused}} {
		if _, err := os.Lstat(crwconfig.JoinRoot(probe.dir, name)); err == nil {
			return probe.state, "", nil
		} else if !os.IsNotExist(err) {
			return "", "", err
		}
	}
	if _, err := os.Lstat(crwconfig.JoinRoot(dir, name)); err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", err
	}
	st, err := pumpLoadState(cfg)
	if err != nil {
		return "", "", err
	}
	if slices.Contains(st.QueueAttempt[thread].Names, name) || slices.Contains(st.QueueAccepted[thread], name) {
		return summaryStateInFlight, "", nil
	}
	return summaryStatePending, "", nil
}

// pumpSummaryWithoutSent drops from the queued names the summary notices sent/ already holds: such a copy was delivered, so it is no part
// of a batch. The queued file is removed, since sent/ is the notice's record.
func pumpSummaryWithoutSent(cfg *Config, dir, thread string, names []string) []string {
	kept := names[:0:0]
	for _, name := range names {
		if summaryIsLogical(strings.TrimSuffix(name, ".txt")) {
			if _, err := os.Lstat(crwconfig.JoinRoot(dir, pumpSentDir, name)); err == nil {
				if err := os.Remove(crwconfig.JoinRoot(dir, name)); err != nil && !os.IsNotExist(err) {
					pumpLog(cfg, fmt.Sprintf("queue %s: the duplicate of the delivered summary notice %s could not be removed: %v", thread, name, err))
					continue
				}
				pumpLog(cfg, fmt.Sprintf("queue %s: the summary notice %s was delivered already; its queued copy is dropped", thread, name))
				continue
			}
		}
		kept = append(kept, name)
	}
	return kept
}
