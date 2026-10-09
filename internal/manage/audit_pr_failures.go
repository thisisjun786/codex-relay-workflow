package manage

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
	"sort"
	"strings"
)

// auditPRFailuresFile is the append-only record of the pull request targets a run could not
// audit, below the audit state directory beside the ledger. The ledger holds ok rows and
// graded results only; a target that failed before it was graded leaves a line here, so the
// next selection can put it behind the targets that never failed (CRW-963).
const auditPRFailuresFile = "audit-pr-failures.jsonl"

// auditPRFailureLimit is how many failures at one head take a target out of the selection
// until its head changes.
const auditPRFailureLimit = 3

// auditPRFailureReasonLimit bounds the reason one line keeps, so a failing command that
// prints without end cannot grow the record.
const auditPRFailureReasonLimit = 500

// auditPRFailureRow is one line of the failure record: the target (the ledger subject), the
// head it failed at, when, and why.
type auditPRFailureRow struct {
	Target string `json:"target"`
	Head   string `json:"head"`
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// auditPRSkip is a target the selection left out because it failed auditPRFailureLimit times
// at its current head.
type auditPRSkip struct {
	Number   int
	Subject  string
	Head     string
	Failures int
}

// auditPRFailuresPath is the failure record: <state_dir>/audit/audit-pr-failures.jsonl.
func auditPRFailuresPath(e *Env, cfg *Config) string {
	return crwconfig.JoinRoot(auditStateDir(e, cfg), "audit", auditPRFailuresFile)
}

// auditPRReadFailures reads the failure record in file order, which is the order the failures
// happened in. A record that is not there is no failures. A line that is not a whole document
// is the torn tail an interrupted append leaves on its own line, so it is skipped: the lines
// after it are whole and still count, and a target whose failure line was torn is retried.
func auditPRReadFailures(e *Env, cfg *Config) ([]auditPRFailureRow, error) {
	data, err := os.ReadFile(auditPRFailuresPath(e, cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rows []auditPRFailureRow
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row auditPRFailureRow
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.Target == "" {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// auditPRRecordFailure appends one failure line. The file is opened for append and only ever
// grows, as the ledger is, and is private to the owner.
func auditPRRecordFailure(e *Env, cfg *Config, target auditPRTarget, cause error) error {
	path := auditPRFailuresPath(e, cfg)
	if err := os.MkdirAll(rootDir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	reason := cause.Error()
	if len(reason) > auditPRFailureReasonLimit {
		reason = reason[:auditPRFailureReasonLimit]
	}
	row := auditPRFailureRow{
		Target: auditPRSubject(target.Number), Head: target.Merge,
		At: e.Now().UTC().Format(auditTimeFormat), Reason: reason,
	}
	err = auditAppendLine(file, path, row)
	return errors.Join(err, file.Close())
}

// auditPRFailureState is what the selection reads from the failure record for one target at
// one head: how many times it failed there, and the position of the last such failure in the
// record (a later position is a later failure). Failures at another head are not counted: a
// new head is new work.
func auditPRFailureState(failures []auditPRFailureRow, subject, head string) (count, last int) {
	last = -1
	for i, row := range failures {
		if row.Target == subject && row.Head == head {
			count++
			last = i
		}
	}
	return count, last
}

// auditPRSkips lists the targets the record holds out of the selection: not audited, and
// failed auditPRFailureLimit times at their head. heads names the current head of each
// subject the caller knows; a subject it does not name is judged at the head of its most
// recent failure, so a report built without the pull request list still names the targets.
func auditPRSkips(failures []auditPRFailureRow, audited map[string]bool, heads map[string]string) []auditPRSkip {
	latest := map[string]string{}
	for _, row := range failures {
		latest[row.Target] = row.Head
	}
	var out []auditPRSkip
	for subject, head := range latest {
		if audited[subject] {
			continue
		}
		if known, ok := heads[subject]; ok {
			head = known
		}
		count, _ := auditPRFailureState(failures, subject, head)
		if count < auditPRFailureLimit {
			continue
		}
		var number int
		if _, err := fmt.Sscanf(subject, auditPRBundlePrefix+"%d", &number); err != nil {
			continue
		}
		out = append(out, auditPRSkip{Number: number, Subject: subject, Head: head, Failures: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// auditPRFailTarget names one target that could not be audited on stderr and records the
// failure for the next selection. A dry run records nothing: it writes no state.
func auditPRFailTarget(e *Env, cfg *Config, target auditPRTarget, cause error, record bool) {
	auditPRSkipTarget(e, target, cause)
	if !record {
		return
	}
	if err := auditPRRecordFailure(e, cfg, target, cause); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit pr: #%d: the failure could not be recorded: %v\n", target.Number, err)
	}
}
