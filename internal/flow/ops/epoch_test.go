package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestReleaseAfterTheEpochWasLostIsNotReleasedWithTheReason (CRW-1285 (1)): a release the relay refuses because another session claimed
// the plan (ErrEpochLost) is answered as a node not released with the reason epoch_lost, and its row keeps the relay's detail; the lane
// ends on it. Any other error of the relay is the op's error, as before.
func TestReleaseAfterTheEpochWasLostIsNotReleasedWithTheReason(t *testing.T) {
	w := newWorld(t)
	w.ledgered(true, designOf("A"))
	w.relay.releaseErr = fmt.Errorf("%w plan-1 (another session claimed it): the run stops: stale_coordinator_epoch: a newer coordinator session holds the plan", ErrEpochLost)
	var rel ReleaseOutcome
	_ = json.Unmarshal(w.must("release", "plan-1", "A", "M"), &rel)
	row := w.j.Events()[len(w.j.Events())-1]
	if rel.Released || rel.Reason != ReasonEpochLost || row.Data["reason"] != ReasonEpochLost || !strings.Contains(dataStr(row, "detail"), "refused:epoch_lost:") ||
		!strings.Contains(dataStr(row, "detail"), "stale_coordinator_epoch") {
		t.Errorf("a release after the epoch was lost: %+v %v", rel, row.Data)
	}
	if ReasonOf(ErrEpochLost.Error()) != ReasonEpochLost {
		t.Errorf("the kind's message names %q", ReasonOf(ErrEpochLost.Error()))
	}
	w.relay.releaseErr = errors.New("the store is gone")
	if _, err := w.op("release", "plan-1", "A", "M"); err == nil || errors.Is(err, ErrEpochLost) {
		t.Errorf("another error of the relay: %v", err)
	}
}
