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
	RequestID, CWD, Prompt, Title, Sandbox, Model, ReasoningEffort, Role string
	RuntimeWorkspaceRoots                                                []string
	ExpectedSandboxPolicy                                                map[string]any
}

type SendRequest struct {
	RequestID, ThreadID, Message string
	Settings                     map[string]any
	BeforeStart                  func(context.Context) (map[string]any, error)
	GuardRPCRequests             int
}

type Turn struct{ ID, Status string }

// HostRPC is the scoped, retained host read seam used by the send's BeforeStart guard.
// Implementations route these calls through the same host as SendMessage.
type HostRPC interface {
	HostCall(context.Context, string, map[string]any) (map[string]any, error)
}
