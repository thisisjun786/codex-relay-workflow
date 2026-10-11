package managed

import "context"

// Adapter is the six host operations ManagedStart invokes. The production bridge
// implementation belongs to todo 28; tests provide the same retained-operation
// semantics as the Python managed-start fake.
type Adapter interface {
	RequireLedger(context.Context, map[string]any) error
	LedgerIdentityRecord(context.Context) (map[string]any, error)
	CreateThread(context.Context, CreateThreadRequest) (map[string]any, error)
	GetOperation(context.Context, string) (map[string]any, error)
	SendMessage(context.Context, SendRequest) (map[string]any, error)
	ReadTurn(context.Context, string, string) (*Turn, error)
	HostRPC
}

type CreateThreadRequest struct {
	RequestID, CWD, Prompt, Title, Sandbox, Model, ReasoningEffort, Role, MCPProfile string
	RuntimeWorkspaceRoots                                                            []string
	ExpectedSandboxPolicy                                                            map[string]any
}

type SendRequest struct {
	RequestID, ThreadID, Message string
	Settings                     map[string]any
	BeforeStart                  func(context.Context) (map[string]any, error)
	GuardRPCRequests             int
}

type Turn struct{ ID, Status string }

// HostRPC is the scoped host seam: the reads the send's BeforeStart guard makes and the reads a creation reconciliation makes
// (thread/loaded/list, thread/read, thread/turns/list), and one write, the idempotent thread/name/set of the title that reconciliation
// sets once a standby is accepted. Implementations route these calls through the same host as SendMessage.
type HostRPC interface {
	HostCall(context.Context, string, map[string]any) (map[string]any, error)
}

// CreationHold is the creation's turn watch kept open after CreateThread has answered. The bridge opens
// a watch on the thread it creates and lets it go as the creation returns; an adapter that implements
// HeldCreator hands it over instead, so the subscription that carries the standby turn's turn/completed
// stays open while the engine waits for that turn to end. Release lets the watch go (the bridge's
// finishSubscription); it is idempotent, and the engine calls it once the wait is over.
type CreationHold interface {
	// TurnCompleted reports whether the watch has received turn/completed for the turn.
	TurnCompleted(turn string) bool
	Release()
}

// HeldCreator is an Adapter whose CreateThread can keep the creation watch open. The engine uses it when
// the adapter offers it and falls back to CreateThread (and the polling read of the standby turn) when not.
type HeldCreator interface {
	CreateThreadHeld(context.Context, CreateThreadRequest) (map[string]any, CreationHold, error)
}

// MCPChecker is an Adapter that knows how the request's MCP profile resolves against the host. MCPProblem reports
// "mcpServers" when the thread the host holds loaded reports other MCP servers than the profile of the child's role
// asks for (a host ignores the overrides of a resume on a loaded thread), and "" when it holds them, when the request
// states no profile the role declares, or when the answer cannot decide. An adapter without it leaves the MCP servers
// out of the orphan judgement; the business guard still refuses a thread that reports other servers.
type MCPChecker interface {
	MCPProblem(ctx context.Context, thread string, settings map[string]any) (string, error)
}
