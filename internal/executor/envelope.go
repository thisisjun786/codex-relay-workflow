package executor

import "time"

type Kind string

const (
	Codex      Kind = "codex"
	Devin      Kind = "devin"
	ClaudeCode Kind = "claude-code"
)

const (
	TaskSchema   = "crw-executor-task/1"
	ResultSchema = "crw-executor-result/1"
)

type Identity struct {
	TaskID               string `json:"task_id"`
	ExecutionID          string `json:"execution_id"`
	Attempt              int64  `json:"attempt"`
	AssignmentGeneration int64  `json:"assignment_generation"`
	Plan                 string `json:"plan"`
	Node                 string `json:"node"`
	Revision             string `json:"revision"`
	CriteriaDigest       string `json:"criteria_digest"`
}

type Repository struct {
	Forge      string `json:"forge"`
	ID         string `json:"id"`
	BaseCommit string `json:"base_commit"`
	TargetRef  string `json:"target_ref"`
	TaskBranch string `json:"task_branch"`
}

type Region struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Key        string `json:"key,omitempty"`
	Change     string `json:"change"`
	Exclusive  bool   `json:"exclusive"`
	Grade      string `json:"grade,omitempty"`
	Rule       string `json:"rule,omitempty"`
}

type Binding struct {
	HostID                 string `json:"host_id"`
	Incarnation            string `json:"incarnation"`
	Kind                   Kind   `json:"kind"`
	CapabilityRevision     string `json:"capability_revision"`
	LaunchProfileDigest    string `json:"launch_profile_digest"`
	ToolchainProfileDigest string `json:"toolchain_profile_digest"`
}

type Requested struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Input struct {
	Kind        string     `json:"kind"`
	Prompt      string     `json:"prompt"`
	BriefDigest string     `json:"brief_digest"`
	Artifacts   []Artifact `json:"artifacts"`
}

type Resources struct {
	CPU         int   `json:"cpu"`
	MemoryBytes int64 `json:"memory_bytes"`
	Heavy       bool  `json:"heavy"`
}

type Check struct {
	ID               string    `json:"id"`
	Args             []string  `json:"args"`
	ExpectedExitCode int       `json:"expected_exit_code"`
	Resources        Resources `json:"resources"`
}

type Task struct {
	Schema        string     `json:"schema"`
	Identity      Identity   `json:"identity"`
	Repository    Repository `json:"repository"`
	Regions       []Region   `json:"regions"`
	WorkKind      string     `json:"work_kind"`
	Executor      Binding    `json:"executor"`
	Requested     Requested  `json:"requested"`
	Input         Input      `json:"input"`
	Checks        []Check    `json:"checks"`
	NotApplicable string     `json:"not_applicable,omitempty"`
}

type Admission struct {
	Task       Task   `json:"task"`
	TaskDigest string `json:"task_digest"`
}

type Grant struct {
	ExecutionID          string    `json:"execution_id"`
	AssignmentGeneration int64     `json:"assignment_generation"`
	Owner                string    `json:"owner"`
	OwnerEpoch           int64     `json:"owner_epoch"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type Heartbeat struct {
	ExecutionID          string    `json:"execution_id"`
	HostID               string    `json:"host_id"`
	Incarnation          string    `json:"incarnation"`
	AssignmentGeneration int64     `json:"assignment_generation"`
	ObservedAt           time.Time `json:"observed_at"`
}

type Observation struct {
	Value         string `json:"value,omitempty"`
	UnknownReason string `json:"unknown_reason,omitempty"`
}

type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

type Provenance struct {
	Provider      Kind        `json:"provider"`
	NativeSession Observation `json:"native_session"`
	NativeTurns   []string    `json:"native_turns"`
	Model         Observation `json:"model"`
	Effort        Observation `json:"effort"`
	CPUMethod     Observation `json:"cpu_method"`
	Tools         []Tool      `json:"tools"`
}

type Candidate struct {
	Branch string `json:"branch"`
	Head   string `json:"head"`
	Tree   string `json:"tree"`
}

type CheckResult struct {
	ID       string `json:"id"`
	ExitCode int    `json:"exit_code"`
	Outcome  string `json:"outcome"`
}

type Payload struct {
	Narrative string        `json:"narrative"`
	Candidate *Candidate    `json:"candidate,omitempty"`
	Checks    []CheckResult `json:"checks"`
	Artifacts []Artifact    `json:"artifacts"`
}

type Result struct {
	Schema     string     `json:"schema"`
	Identity   Identity   `json:"identity"`
	TaskDigest string     `json:"task_digest"`
	Executor   Binding    `json:"executor"`
	Outcome    string     `json:"outcome"`
	Provenance Provenance `json:"provenance"`
	Payload    Payload    `json:"payload"`
}

type Peer struct {
	HostID      string `json:"host_id"`
	Incarnation string `json:"incarnation"`
}

type Limits struct {
	EnvelopeBytes      int64 `json:"envelope_bytes"`
	ArtifactBytes      int64 `json:"artifact_bytes"`
	TotalArtifactBytes int64 `json:"total_artifact_bytes"`
	ArtifactCount      int   `json:"artifact_count"`
}

var DefaultLimits = Limits{EnvelopeBytes: 1 << 20, ArtifactBytes: 8 << 20, TotalArtifactBytes: 32 << 20, ArtifactCount: 64}
