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

// PolicyReport is what `crw flow roles` shows: where the policy was read from, its digest, and the pair of each role
// and the alias table a call is resolved against.
type PolicyReport struct {
	// Source and Digest are null for the placeholder table of the fake backend.
	Source      *string       `json:"source"`
	Digest      *string       `json:"digest"`
	Mode        string        `json:"mode"`
	Placeholder bool          `json:"placeholder"`
	Roles       []RoleReport  `json:"roles"`
	Aliases     []AliasReport `json:"aliases"`
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
		report.Roles = append(report.Roles, RoleReport{Role: name, Family: spec.Family, Model: spec.Pair.Model, ReasoningEffort: spec.Pair.Effort, Sandbox: spec.Sandbox, Aliases: spec.ModelAliases, AutoCompactTokenLimit: spec.AutoCompactTokenLimit})
	}
	for _, row := range table.Aliases {
		report.Aliases = append(report.Aliases, AliasReport{Key: row.Key, Effort: row.Effort, Isolation: row.Isolation, Role: row.Role, Refuse: row.Cause, Detail: row.Detail})
	}
	return report
}

// Text renders the report for a terminal.
func (r PolicyReport) Text() string {
	var b strings.Builder
	if r.Placeholder {
		b.WriteString("policy: placeholder table of the fake backend (CODEX_THREAD_BRIDGE_EXECUTION_POLICY names no file)\n")
	} else {
		fmt.Fprintf(&b, "policy: %s\ndigest: %s\n", *r.Source, *r.Digest)
	}
	fmt.Fprintf(&b, "mode: %s\n\nroles:\n", r.Mode)
	for _, role := range r.Roles {
		limit := ""
		if role.AutoCompactTokenLimit != nil {
			limit = fmt.Sprintf(" autoCompactTokenLimit %d", *role.AutoCompactTokenLimit)
		}
		fmt.Fprintf(&b, "  %-15s %s at %s (%s, %s)%s aliases: %s\n", role.Role, role.Model, role.ReasoningEffort, role.Family, role.Sandbox, limit, strings.Join(role.Aliases, ", "))
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
	return b.String()
}
