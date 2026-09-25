package bridge

import (
	"context"
	"regexp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

var hostVersionToken = regexp.MustCompile(`/([0-9]+(?:\.[0-9]+)+[^\s()/;,]*)`)

// testedHostVersion is bridge.py TESTED_HOST_VERSION; the two readings below are dated by the
// host they were taken on, which is deliberately a separate constant each.
const (
	testedHostVersion          = "0.154.0"
	desktopVisibilityHost      = "0.153.4"
	approvalPreservationHost   = "0.154.0"
	capabilitiesNote           = "What this bridge implements, and nothing else. Every value is true by reading this bridge's own code, so none of it was asked of the connected host and none of it is an answer about one. The two goal questions are answered precisely in exposure, as goalObjectiveWrite and goalPause. Questions about the host that nothing here asked are in hostNotProbed, which carries no values at all."
	hostNotProbedNote          = "Questions this tool does not answer. No capability probe is performed, so each entry is a sentence and not a value: there is nothing here to read as true or false. hostSupport.state compares the connected server's version against the one this bridge was built against, and does not reach these questions, which are unanswered on every version including that one."
	desktopManagedWorktreesNot = "Not asked. Worktrees made through this bridge are bridge-managed, which is reported as bridgeManagedWorktrees. Whether the connected host offers a Desktop-managed worktree of its own is carried by a host feature flag rather than an App Server method, so this transport never sees it, and a flag that is switched off is not a capability absent."
	desktopProjectRegistryNot  = "Not asked. This bridge imports and creates no project, which is reported as projectImport, and it uses only a project id a caller supplies. What the connected host's project registry holds is not read here, and an empty backend listing seen once on one setup is not a statement that the registry is missing."
	approvalPreservation       = "send_message_to_thread omits approvalPolicy from thread/resume, so it cannot set or change the policy of a thread it did not create. That is true by reading this bridge; what the host does with the omission was read once, and is reported as its own observation."
	approverRoute              = "The host's, not this bridge's. Measured on codex-cli " + testedHostVersion + ", the host sends an approval request to every client subscribed to the thread, replays a pending one to a client that resumes the thread later, and applies the first answer from any of them. This bridge therefore answers none and the thread's own client decides; that it does so on the connected server is not re-measured by this call, and how a particular client such as Desktop presents the request is not established here."
)

func connectedVersion(agent, version string) bool {
	for _, match := range hostVersionToken.FindAllStringSubmatch(agent, -1) {
		if match[1] == version {
			return true
		}
	}
	return false
}

// GetCapabilities reports bridge exposure separately from version-scoped host observations.
// Connecting is the only host interaction; no capability probe is made.
func (b *Bridge) GetCapabilities(ctx context.Context) (map[string]any, error) {
	client, ok := b.RPC.(*appserver.Client)
	if !ok {
		return nil, &Invalid{"capabilities require an App Server connection"}
	}
	if err := client.Connect(ctx); err != nil {
		return nil, err
	}
	server := client.Info()
	agent := text(server["userAgent"])
	observed := func(version, statement string) map[string]any {
		return map[string]any{"observedOn": "codex-cli " + version, "observedServer": agent, "sameVersionConnected": connectedVersion(agent, version), "observation": statement, "note": "A reading taken on the named host version, not a probe of the server connected now. sameVersionConnected is the only measured value here."}
	}
	state := "unknown_host_version"
	if connectedVersion(agent, testedHostVersion) {
		state = "tested"
	}
	return map[string]any{
		"server": server, "transport": "same-host Unix WebSocket", "socket": client.SocketPath(),
		"capabilities":     map[string]any{"createThread": true, "sendMessage": true, "listReadWait": true, "goalRead": true, "bridgeManagedWorktrees": true, "projectImport": false, "clientSideToolsAndApprovals": false},
		"capabilitiesNote": capabilitiesNote,
		"executionPolicy":  b.Policy.Summary(),
		"approvals": map[string]any{
			"declarable":              []any{"never", "on-request", "untrusted"},
			"transmitsApprovalPolicy": false,
			"preservation":            approvalPreservation,
			"preservationObserved":    observed(approvalPreservationHost, "Read in both directions: the resume reports the thread's own policy and does not inherit the CODEX_HOME config default."),
			"servicesApprovals":       false,
			"onApprovalRequest":       "left_for_thread_approver",
			"approverRoute":           approverRoute,
			"limits":                  settings.ApprovalLimits,
		},
		"exposure":          map[string]any{"steerActiveTurn": true, "goalPause": true, "goalObjectiveWrite": false, "turnInterrupt": false, "turnQueue": false, "note": "Tool exposure by this bridge version. Not a probe of the connected host."},
		"hostSupport":       map[string]any{"testedHost": "codex-cli " + testedHostVersion, "observedServer": agent, "state": state, "steerActiveTurn": "turn/steer, requires expectedTurnId", "goalPause": "thread/goal/set, status paused", "note": "Generated from the tested host's protocol. No live capability probe is performed, and an unrecognised server is reported unknown rather than assumed."},
		"hostNotProbed":     map[string]any{"desktopManagedWorktrees": desktopManagedWorktreesNot, "desktopProjectRegistry": desktopProjectRegistryNot},
		"hostNotProbedNote": hostNotProbedNote,
		"desktopVisibility": observed(desktopVisibilityHost, "Threads created in an existing project checkout appeared in Desktop. Verify the actual Desktop listing for each launch; backend project IDs are separate."),
	}, nil
}
