package argparse

import "slices"

// ReadOnlyCommands is cli.py's READ_ONLY_COMMANDS.
var ReadOnlyCommands = []string{
	"managed-show", "reporting-show", "reporting-derive", "settings-show",
	"linkage-outstanding", "linkage-completion", "linkage-down", "linkage-up", "linkage-counterpart",
	"supervisor-select", "supervisor-standing", "supervisor-show", "ack-proof",
	"criteria-show", "revision-head", "assignment-show", "assignment-find",
	"dispositions-show", "sync-status", "sync-next", "sync-operation", "fault-show", "fault-next",
	"fault-attention", "fault-notifications", "product-show", "route-show",
	"show", "status", "doctor", "store-identity",
	"intent-show", "guard-evaluate", "merge-turn-show", "capacity-show", "region-show",
	"packet-check", "merge-evidence",
}

// ReadOnlyForm is cli.py's _read_only_command for the selected subcommand line (the command
// name first): the READ_ONLY_COMMANDS set, fault-policy without --fault-class, fault-limit
// without --kind, service status, and store-challenge with --read. Every entry point that
// serves a relay command line classifies it here, so the relay CLI and a domain package's own
// console agree on which forms open the store lazily and never create it.
func ReadOnlyForm(line []string) bool {
	if len(line) == 0 {
		return false
	}
	command, rest := line[0], line[1:]
	switch command {
	case "fault-policy":
		return !Parse(command, rest).Given["fault-class"]
	case "fault-limit":
		return !Parse(command, rest).Given["kind"]
	case "store-challenge":
		return Parse(command, rest).Given["read"]
	case "service":
		parent := Parse(command, rest)
		return len(parent.Remaining) > 0 && parent.Remaining[0] == "status"
	}
	return slices.Contains(ReadOnlyCommands, command)
}
