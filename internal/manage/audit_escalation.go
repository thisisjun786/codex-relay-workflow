package manage

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
)

// The kind an audit ledger line can carry besides a graded result. A graded result line has no
// kind. A line of another kind is not a result: every reader of the ledger's results skips it,
// so the results are read as they were before any such line existed (CRW-962).
const auditLedgerKindEscalationPosted = "escalation_posted"

// auditEscalationRow is the ledger line that records a severity raise of an already posted
// draft as posted: the management session raised the issue it had opened from `from` to `to`.
// target is the draft's fingerprint, and ref names the comment or field the raise was posted
// in when that is known. The line is appended and never rewritten.
type auditEscalationRow struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	From   string `json:"from"`
	To     string `json:"to"`
	At     string `json:"at"`
	Ref    string `json:"ref"`
}

// auditLedgerLine is one decoded ledger line: a graded result, a posted escalation, or a line
// of a kind this build does not read (which is neither, and is skipped by its readers).
type auditLedgerLine struct {
	Kind       string
	Result     auditLedgerRow
	Escalation auditEscalationRow
}

// auditLedgerDecode decodes one ledger line. A line that is not a whole JSON document is an
// error, so each reader keeps its own handling of a torn line.
func auditLedgerDecode(line string) (auditLedgerLine, error) {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		return auditLedgerLine{}, err
	}
	out := auditLedgerLine{Kind: head.Kind}
	switch head.Kind {
	case "":
		return out, json.Unmarshal([]byte(line), &out.Result)
	case auditLedgerKindEscalationPosted:
		return out, json.Unmarshal([]byte(line), &out.Escalation)
	}
	return out, nil
}

// auditEscalationPosted is the highest severity each target has been posted at, by the
// rank auditDraftSeverityRank gives (a lower rank is more severe). A row with a severity this
// build does not know is ignored: it covers nothing.
func auditEscalationPosted(rows []auditEscalationRow) map[string]auditEscalationRow {
	best := map[string]auditEscalationRow{}
	for _, row := range rows {
		rank, known := auditDraftSeverityRank[row.To]
		if !known || row.Target == "" {
			continue
		}
		if have, ok := best[row.Target]; ok && auditDraftSeverityRank[have.To] <= rank {
			continue
		}
		best[row.Target] = row
	}
	return best
}

// auditEscalationAppend appends one posted-escalation line to the ledger. The ledger is
// opened for append like every other writer of it, so a concurrent writer's lines are never
// rewritten, and an earlier torn write gets its line feed first.
func auditEscalationAppend(e *Env, cfg *Config, row auditEscalationRow) error {
	row.Kind = auditLedgerKindEscalationPosted
	dir := crwconfig.JoinRoot(auditStateDir(e, cfg), "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := crwconfig.JoinRoot(dir, auditLedgerFile)
	ledger, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = auditAppendLine(ledger, path, row)
	return errors.Join(err, ledger.Close())
}

// auditDraftMarkPosted records that the management session posted the raise of one draft's
// severity, and returns the line it wrote. It writes nothing and succeeds when the ledger
// already holds a posted line that covers the raise (the same mark again), and refuses a draft
// that is not posted or a draft the audits have not reported above the severity its issue
// stands at. The draft file is never written: a posted draft is the record of its issue.
// The caller holds the drafts lock.
func auditDraftMarkPosted(e *Env, cfg *Config, fingerprint, ref string) (*auditEscalationRow, error) {
	if err := auditDraftFingerprintName(fingerprint); err != nil {
		return nil, err
	}
	doc, err := auditDraftLoad(crwconfig.JoinRoot(auditDraftDir(e, cfg), fingerprint+".json"))
	if err != nil {
		return nil, err
	}
	if doc.State != auditDraftStatePosted {
		return nil, fmt.Errorf("not_posted: the draft %s is %s; mark it posted before recording a raise", fingerprint, doc.State)
	}
	section, err := auditDraftSectionOf(cfg)
	if err != nil {
		return nil, err
	}
	// Every severity is considered: the raise is the highest the audits reported for this defect,
	// whatever threshold a drafts run reports at.
	collected, err := auditDraftCollect(e, cfg, section, auditDraftScope{}, auditDraftSeverityRank["P3"])
	if err != nil {
		return nil, err
	}
	standing := doc.Severity
	posted, have := auditEscalationPosted(collected.escalations)[fingerprint]
	if have && auditDraftSeverityRank[posted.To] < auditDraftSeverityRank[standing] {
		standing = posted.To
	}
	candidate, found := collected.candidates[fingerprint]
	if found && auditDraftSeverityRank[candidate.severity] < auditDraftSeverityRank[standing] {
		row := auditEscalationRow{
			Kind: auditLedgerKindEscalationPosted, Target: fingerprint, From: standing, To: candidate.severity,
			At: e.Now().UTC().Format(auditTimeFormat), Ref: ref,
		}
		if err := auditEscalationAppend(e, cfg, row); err != nil {
			return nil, err
		}
		return &row, nil
	}
	if have {
		return nil, nil
	}
	return nil, fmt.Errorf("no_escalation: no audit reported the draft %s above P%s", fingerprint, standing[1:])
}
