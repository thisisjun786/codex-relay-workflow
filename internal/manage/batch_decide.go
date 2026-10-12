package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
)

// crw manage batch decide (CRW-1205). The manager chose the issues of the night with `crw manage batch plan` and now records the
// decision: the selected issues, the token budget of the run and its deadline. The record is a user-decision record of the relay
// (kind policy, authority delegated-management, options run and skip) whose context is the night batch document
// (flowcli.NightBatch), raised and answered `run` through the relay's own decision commands. `crw flow start night-batch --decision D`
// starts the run from it. Nothing is released here, and nothing is started: releases happen only inside the run.

const batchDecideUsage = "usage: crw manage batch decide --plan FILE --flow-plan PLAN --milestone M --token-budget N --deadline RFC3339 [--cap N] [--by TASK]"

// batchDecideConfig is where the command reads its configuration; a test replaces it to point the relay helper at a fake.
var batchDecideConfig = coreDefaults

func batchRunDecide(ctx context.Context, e *Env, args []string) int {
	if auditOptionsHelpRequested(args) {
		fmt.Fprintln(e.Stdout, batchDecideUsage)
		return 0
	}
	if code, refused := batchConfigRefused(e); refused {
		return code
	}
	values, err := auditPkgParseArgs(args, map[string]bool{"plan": true, "flow-plan": true, "milestone": true, "token-budget": true, "deadline": true, "cap": true, "by": true})
	if err != nil {
		return batchRefuse(e, batchDecideUsage, "decide", err)
	}
	for _, name := range []string{"plan", "flow-plan", "milestone", "token-budget", "deadline"} {
		if strings.TrimSpace(values[name]) == "" {
			return batchRefuse(e, batchDecideUsage, "decide", fmt.Errorf("the option --%s needs a value", name))
		}
	}
	raw, err := batchReadInput(e, values["plan"])
	if err != nil {
		return batchRefuse(e, batchDecideUsage, "decide", err)
	}
	var plan BatchPlanDocument
	if err := json.Unmarshal(raw, &plan); err != nil || plan.Schema != batchPlanSchema {
		return batchRefuse(e, batchDecideUsage, "decide", fmt.Errorf("the plan is not a %s document (crw manage batch plan prints one)", batchPlanSchema))
	}
	budget, err := strconv.ParseInt(strings.TrimSpace(values["token-budget"]), 10, 64)
	if err != nil || budget <= 0 {
		return batchRefuse(e, batchDecideUsage, "decide", errors.New("--token-budget is a number of tokens above zero: a night run without a ceiling is not started"))
	}
	cap := 0
	if v := strings.TrimSpace(values["cap"]); v != "" {
		if cap, err = strconv.Atoi(v); err != nil || cap < 0 {
			return batchRefuse(e, batchDecideUsage, "decide", errors.New("--cap is the number of executors at once, 0 or more"))
		}
	}
	night := flowcli.NightBatch{Schema: flowcli.NightBatchSchema, Label: plan.Label, Plan: strings.TrimSpace(values["flow-plan"]), Milestone: strings.TrimSpace(values["milestone"]),
		TokenBudget: budget, Deadline: strings.TrimSpace(values["deadline"]), Cap: cap}
	for _, s := range plan.Selected {
		night.Issues = append(night.Issues, s.Issue)
	}
	if err := night.Validate(); err != nil {
		return batchRefuse(e, batchDecideUsage, "decide", err)
	}
	if at, _ := time.Parse(time.RFC3339, night.Deadline); !at.After(e.Now()) {
		return batchRefuse(e, batchDecideUsage, "decide", fmt.Errorf("the deadline %s has passed", night.Deadline))
	}
	by := strings.TrimSpace(values["by"])
	if by == "" && e.Getenv != nil {
		by = strings.TrimSpace(e.Getenv("CODEX_THREAD_ID"))
	}
	if by == "" {
		return batchRefuse(e, batchDecideUsage, "decide", errors.New("--by names the manager task that records the decision (default: CODEX_THREAD_ID)"))
	}
	document, _ := json.Marshal(night)
	cfg := batchDecideConfig(e)
	out, code, err := e.Relay(ctx, cfg, "decision-raise", "--kind", "policy", "--context", string(document),
		"--option", "run=Run the night batch:crw flow start night-batch --decision starts the run for the selected issues",
		"--option", "skip=Skip tonight:nothing is started and the issues stay in Backlog",
		"--blocking", "project="+night.Plan, "--needed-by", night.Deadline, "--origin-project", night.Plan,
		"--source", "night_batch="+night.Label, "--authority", "delegated-management", "--raised-via", "crw-manage-batch-decide")
	if err != nil || code != 0 {
		return batchDecideFail(e, "decision-raise", out, code, err)
	}
	var raised struct {
		DecisionID string `json:"decisionId"`
		State      string `json:"state"`
		Merged     bool   `json:"merged"`
	}
	if err := json.Unmarshal(out, &raised); err != nil || raised.DecisionID == "" {
		fmt.Fprintf(e.Stderr, "crw manage batch decide: error: the relay's answer to decision-raise names no decision: %q\n", strings.TrimSpace(string(out)))
		return 3
	}
	state := raised.State
	if state != "answered" && state != "applied" {
		out, code, err = e.Relay(ctx, cfg, "decision-answer", "--decision", raised.DecisionID, "--option", flowcli.NightRun, "--by", by, "--via", "management-message", "--authority", "delegated-management")
		if err != nil || code != 0 {
			return batchDecideFail(e, "decision-answer", out, code, err)
		}
		state = "answered"
	}
	doc, _ := json.Marshal(map[string]any{
		"schema": "crw-batch-decision/1", "decision_id": raised.DecisionID, "state": state, "already_recorded": raised.Merged, "label": night.Label, "issues": night.Issues,
		"token_budget": night.TokenBudget, "deadline": night.Deadline, "start": "crw flow start night-batch --decision " + raised.DecisionID + " --thread <orchestrator thread>",
	})
	fmt.Fprintf(e.Stdout, "%s\n", doc)
	return 0
}

// batchDecideFail reports a relay command that failed: the relay's own refusal when it printed one.
func batchDecideFail(e *Env, command string, out []byte, code int, err error) int {
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage batch decide: error: %s: %v\n", command, err)
		return 3
	}
	fmt.Fprintf(e.Stderr, "crw manage batch decide: error: the relay refused %s (exit %d): %s\n", command, code, strings.TrimSpace(string(out)))
	return 3
}
