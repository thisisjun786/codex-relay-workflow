package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	flowdelivery "github.com/thisisjun786/codex-relay-workflow/internal/flow/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/planinput"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/pullrequest"
	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/publication"
	trackingref "github.com/thisisjun786/codex-relay-workflow/internal/tracking/ref"
)

// deliveryReport is the JSON document `crw flow audit --delivery` prints and the
// delivery_checked row of the run holds.
type deliveryReport struct {
	Run          string                     `json:"run"`
	Milestone    string                     `json:"milestone"`
	Head         string                     `json:"head"`
	State        string                     `json:"state"`
	Criteria     []flowdelivery.Criterion   `json:"criteria"`
	Observations []flowdelivery.Observation `json:"observations"`
	CheckedAt    string                     `json:"checked_at"`
	Recorded     bool                       `json:"recorded"`
}

// DefaultDeliveryReader is the production readback of --delivery: exact Git
// reads of the run's own checkout (no fetch, no push) and tracking reads through
// the publication factory. A service it cannot reach is an error to the caller,
// never an affirmative answer.
func DefaultDeliveryReader() *flowdelivery.Reader {
	return &flowdelivery.Reader{
		Remote: func(ctx context.Context, checkout, branch string) (string, error) {
			cmd := exec.CommandContext(ctx, "git", "-C", checkout, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
			cmd.Env = gitprobe.Sanitize(nil, "GIT_TERMINAL_PROMPT=0")
			out, err := cmd.Output()
			if err != nil {
				return "", fmt.Errorf("origin's branch could not be read")
			}
			fields := strings.Fields(string(out))
			if len(fields) != 2 || fields[1] != "refs/heads/"+branch || !isCommitSHA(fields[0]) {
				return "", fmt.Errorf("origin's branch answer was not a head")
			}
			return fields[0], nil
		},
		Ancestor: func(ctx context.Context, checkout, base, observed string) (bool, error) {
			cmd := exec.CommandContext(ctx, "git", "-C", checkout, "merge-base", "--is-ancestor", base, observed)
			cmd.Env = gitprobe.Sanitize(nil, "GIT_TERMINAL_PROMPT=0")
			if err := cmd.Run(); err == nil {
				return true, nil
			} else if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return false, nil
			} else {
				return false, fmt.Errorf("containment could not be read")
			}
		},
		Issue: func(ctx context.Context, ref string) (string, string, error) {
			r, err := trackingref.Parse(ref)
			if err != nil {
				return "", "", err
			}
			g, err := publication.NewGitHub(r.Repo)
			if err != nil {
				return "", "", err
			}
			is, err := g.Issue(ctx, r)
			if err != nil {
				return "", "", err
			}
			if !strings.EqualFold(is.Ref.Repo, r.Repo) || is.Ref.Number != r.Number {
				return "", "", fmt.Errorf("the tracker answered a different issue")
			}
			return string(is.State), string(is.CloseReason), nil
		},
		Milestone: func(ctx context.Context, ref string) (string, error) {
			r, err := trackingref.Parse(ref)
			if err != nil {
				return "", err
			}
			g, err := publication.NewGitHub(r.Repo)
			if err != nil {
				return "", err
			}
			ms, _, err := g.Milestone(ctx, r)
			if err != nil {
				return "", err
			}
			if !strings.EqualFold(ms.Ref.Repo, r.Repo) || ms.Ref.Number != r.Number {
				return "", fmt.Errorf("the tracker answered a different milestone")
			}
			return string(ms.State), nil
		},
	}
}

func isCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, b := range s {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

// runDeliveryAudit is `crw flow audit --run RUN --delivery`: it reads back the
// explicit delivery criteria of the run's last closed milestone after the owner
// published with existing procedures. It records a run-level delivery_checked
// row; it pushes nothing and closes nothing.
func runDeliveryAudit(ctx context.Context, setup AuditSetup, noRecord, asJSON bool, stdout, stderr io.Writer) (code int) {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "crw flow audit: error: "+format+"\n", a...)
		return 2
	}
	// The journal the lock holds open is the authoritative read, recorded or not:
	// a run still writing owns its journal, and a fresh open sees the rows stored
	// after the run's end row (CRW-1487).
	j, release := setup.Journal, func() error { return nil }
	if setup.Record != nil {
		fresh, ended, rel, err := setup.Record()
		switch {
		case errors.Is(err, ErrRunLive):
			return fail("a process holds the run: it owns its journal")
		case err != nil:
			return fail("the delivery check could not read the journal: %v", err)
		case !ended:
			_ = rel()
			return fail("the run has not finished: it owns its journal")
		}
		j, release = fresh, rel
	} else if !setup.Finished {
		return fail("the run has not finished: it owns its journal")
	}
	events := j.Events()
	facts, err := pullrequest.FactsOf(events)
	if err != nil {
		_ = release()
		return fail("the run's facts could not be read: %v", err)
	}
	b := facts.Body
	if !facts.Closed || b.Gate.State != "pass" || b.Crosscheck.Verdict != "verified" || b.Crosscheck.Head != b.Tip {
		_ = release()
		return fail("run %s has no closed milestone verified at its tip%s", setup.RunID, func() string {
			if facts.Why != "" {
				return " (" + facts.Why + ")"
			}
			return ""
		}())
	}
	var ms ops.Milestone
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.Op != ops.OpMilestone || ev.Event != engine.EvCompleted {
			continue
		}
		s, _ := ev.Data["result"].(string)
		var got ops.Milestone
		if json.Unmarshal([]byte(s), &got) == nil && got.ID == b.Milestone {
			ms = got
		}
	}
	if ms.ID == "" {
		_ = release()
		return fail("run %s holds no completed milestone row of %s", setup.RunID, b.Milestone)
	}
	criteria, _ := flowdelivery.Valid(ms.Criteria, ms.DeliveryCriteria)
	if len(criteria) == 0 {
		_ = release()
		return fail("milestone %s holds no valid delivery_criteria", ms.ID)
	}
	checkout, branch := "", ""
	if setup.DeliveryTarget != nil {
		checkout, branch, _ = setup.DeliveryTarget(facts.Plan)
	}
	var issues []string
	for _, n := range ms.Nodes {
		ref := n.Issue
		if r, err := planinput.DecodeKey(n.Issue); err == nil {
			ref = r.String()
		} else if r, err := trackingref.Parse(n.Issue); err == nil {
			ref = r.String()
		}
		issues = append(issues, ref)
	}
	reader := DefaultDeliveryReader()
	if setup.DeliveryReader != nil {
		reader = setup.DeliveryReader
	}
	target := flowdelivery.Target{Checkout: checkout, Branch: branch, Head: b.Tip, Issues: issues}
	observations := flowdelivery.Observe(ctx, criteria, target, *reader)
	report := deliveryReport{
		Run: setup.RunID, Milestone: ms.ID, Head: b.Tip,
		State:        flowdelivery.State(observations),
		Criteria:     criteria,
		Observations: observations,
		CheckedAt:    AuditNow().UTC().Format(time.RFC3339Nano),
	}
	recorded := false
	if !noRecord {
		j.Append(engine.Event{RunID: setup.RunID, Kind: engine.KindRun, Event: "delivery_checked", Data: map[string]any{
			"run": report.Run, "milestone": report.Milestone, "head": report.Head, "state": report.State,
			"criteria": report.Criteria, "observations": report.Observations, "checked_at": report.CheckedAt,
		}})
		if f, ok := j.(engine.FailingJournal); ok && f.Err() != nil {
			_ = release()
			return fail("the delivery check could not be stored: %v", f.Err())
		}
		recorded = true
	}
	if err := release(); err != nil {
		return fail("the delivery check could not be stored: %v", err)
	}
	report.Recorded = recorded
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Fprintf(stdout, "run %s milestone %s: delivery %s at %s (recorded: %t)\n", report.Run, report.Milestone, report.State, report.Head, recorded)
		for _, o := range observations {
			fmt.Fprintf(stdout, "  %s %s %s: %s\n", o.Criterion, o.Kind, o.State, o.Evidence)
		}
	}
	if report.State != flowdelivery.Passed {
		return 1
	}
	return 0
}
