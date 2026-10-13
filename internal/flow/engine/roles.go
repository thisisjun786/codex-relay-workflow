package engine

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// RoleReport is one role of a PolicyReport.
type RoleReport struct {
	Role            string   `json:"role"`
	Family          string   `json:"family"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoningEffort"`
	Sandbox         string   `json:"sandbox"`
	Aliases         []string `json:"aliases"`
	// AutoCompactTokenLimit is the pair's model_auto_compact_token_limit, absent when the pair declares none.
	AutoCompactTokenLimit *int64 `json:"autoCompactTokenLimit,omitempty"`
	// State is where the pair came from in a table read from the helper role store: store, compiled_default or undecided
	// (CRW-1330); absent for the placeholder table.
	State string `json:"state,omitempty"`
}

// AliasReport is one row of the alias table in a PolicyReport.
type AliasReport struct {
	Key       string `json:"key"`
	Effort    string `json:"effort,omitempty"`
	Isolation string `json:"isolation,omitempty"`
	Role      string `json:"role,omitempty"`
	Refuse    string `json:"refuse,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// PolicyReport is what `crw flow roles` shows: where the role table was read from (the helper role store), its digest (the
// canonical digest of the six pairs the run pins), the pair of each role, the alias table a call is resolved against, and
// the warnings of the table and of the execution policy file.
type PolicyReport struct {
	// Source and Digest are null for the placeholder table of the fake backend.
	Source      *string       `json:"source"`
	Digest      *string       `json:"digest"`
	Mode        string        `json:"mode"`
	Placeholder bool          `json:"placeholder"`
	Roles       []RoleReport  `json:"roles"`
	Aliases     []AliasReport `json:"aliases"`
	// Warnings are the table's warnings and then the execution policy loader's (CRW-1330); absent when there are none.
	Warnings []string `json:"warnings,omitempty"`
}

// DescribePolicy reports the role table of a flow run. exec is the execution policy it was built from (the zero value
// for the placeholder table).
func DescribePolicy(table *Policy, exec execution.Policy) PolicyReport {
	report := PolicyReport{Mode: exec.Mode(), Placeholder: table.Source == "" && table.Digest == ""}
	if !report.Placeholder {
		source, digest := table.Source, table.Digest
		report.Source, report.Digest = &source, &digest
	}
	for _, name := range execution.AgentRoleNames {
		spec, ok := table.Roles[name]
		if !ok {
			continue
		}
		report.Roles = append(report.Roles, RoleReport{Role: name, Family: spec.Family, Model: spec.Pair.Model, ReasoningEffort: spec.Pair.Effort, Sandbox: spec.Sandbox, Aliases: spec.ModelAliases, AutoCompactTokenLimit: spec.AutoCompactTokenLimit, State: table.States[name]})
	}
	report.Warnings = append(append(report.Warnings, table.Warnings...), exec.Warnings()...)
	for _, row := range table.Aliases {
		report.Aliases = append(report.Aliases, AliasReport{Key: row.Key, Effort: row.Effort, Isolation: row.Isolation, Role: row.Role, Refuse: row.Cause, Detail: row.Detail})
	}
	return report
}

// Text renders the report for a terminal.
func (r PolicyReport) Text() string {
	var b strings.Builder
	if r.Placeholder {
		b.WriteString("roles: placeholder table of the fake backend (a test with no helper role store)\n")
	} else {
		fmt.Fprintf(&b, "roles: %s\ndigest: %s\n", *r.Source, *r.Digest)
	}
	fmt.Fprintf(&b, "mode: %s\n\npairs:\n", r.Mode)
	for _, role := range r.Roles {
		limit := ""
		if role.AutoCompactTokenLimit != nil {
			limit = fmt.Sprintf(" autoCompactTokenLimit %d", *role.AutoCompactTokenLimit)
		}
		pair := role.Model + " at " + role.ReasoningEffort
		switch {
		case role.State == "undecided":
			pair = "undecided (inherits the session)"
		case role.ReasoningEffort == "":
			pair = role.Model + " at an undecided effort"
		}
		state := ""
		if role.State == "compiled_default" {
			state = " [compiled default]"
		}
		fmt.Fprintf(&b, "  %-15s %s (%s, %s)%s%s aliases: %s\n", role.Role, pair, role.Family, role.Sandbox, limit, state, strings.Join(role.Aliases, ", "))
	}
	b.WriteString("\naliases:\n")
	for _, row := range r.Aliases {
		call := "model/agentType " + row.Key
		if row.Effort != "" {
			call += ", effort " + row.Effort
		}
		if row.Isolation != "" {
			call += ", isolation " + row.Isolation
		}
		if row.Role != "" {
			fmt.Fprintf(&b, "  %s -> %s\n", call, row.Role)
		} else {
			fmt.Fprintf(&b, "  %s -> refused (%s)\n", call, row.Refuse)
		}
	}
	if len(r.Warnings) > 0 {
		b.WriteString("\nwarnings:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  warning: %s\n", w)
		}
	}
	return b.String()
}
