// Package dag validates DAG plans and keeps them as an append-only revision log in the relay
// store (CRW-183; the contract is the CRW-182 DAG execution contract, docs/relay/dag-plans.md).
//
// A plan is a set of nodes (one deliverable each) and the edges between them. It is never edited:
// a revision is a list of typed changes applied to the plan its parent revision folded to, and the
// current plan is the fold of the log. A revision is validated against the plan it would produce
// before anything is written, so a rejected plan leaves no trace. Nothing here decides what may run
// (the ready set, release and acceptance belong to the scheduler that reads these plans).
package dag

// Schema names of the documents this package reads and writes.
const (
	SchemaRevision = "dag-plan-revision/1"
	SchemaSnapshot = "dag-plan-snapshot/1"
	SchemaSlice    = "dag-node-slice/1"
	SchemaManifest = "dag-input-manifest/1"
)

// Node kinds (contract 1.2).
const (
	NodeImplementation = "implementation" // one issue = one child = one PR
	NodeNonPR          = "non_pr"         // research, design, verification, operation: no PR
)

// Edge kinds (contract 2).
const (
	EdgeArtifactVerified = "artifact_verified"
	EdgeIntegrated       = "integrated"
	EdgeDecision         = "decision"
)

// Change operations.
const (
	OpAddNode     = "add_node"
	OpUpdateNode  = "update_node"
	OpReplaceNode = "replace_node"
	OpRetireNode  = "retire_node"
	OpAddEdge     = "add_edge"
	OpRetireEdge  = "retire_edge"

	// The lifecycle changes (CRW-281, lifecycle.go): pause, resume, cancel and archive a node, and pause or resume the plan.
	OpPauseNode   = "pause_node"
	OpResumeNode  = "resume_node"
	OpCancelNode  = "cancel_node"
	OpArchiveNode = "archive_node"
	OpPausePlan   = "pause_plan"
	OpResumePlan  = "resume_plan"
)

// The graph-size limits a revision is validated against. They bound what one plan may hold; they
// are not the execution limits (how many nodes may run at once), which belong to the scheduler.
const (
	MaxNodes           = 64
	MaxEdges           = 256
	MaxChanges         = 256
	MaxDocumentBytes   = 1 << 20
	MaxIDLength        = 128
	MaxTitleLength     = 256
	MaxTextLength      = 512
	MaxAuthorities     = 16
	MaxAuthorityLength = 256
)

// Node is a node's spec: what the plan says about one deliverable.
type Node struct {
	NodeID            string
	IssueKey          string
	Kind              string
	Title             string
	CriteriaSetDigest string
}

// Edge is an edge's spec. Which of the optional fields apply depends on Kind (contract 2.4).
type Edge struct {
	EdgeID            string
	FromNodeID        string
	ToNodeID          string
	Kind              string
	TargetRepository  string
	TargetBaseRef     string
	PinsCodeHead      bool
	DecisionSubject   string
	DecisionDigest    string
	RequiredAuthority []string
}

// Change is one typed change of a revision. Node is set for add_node, update_node and replace_node,
// Edge for add_edge, NodeID for retire_node and the four node lifecycle changes, EdgeID for retire_edge,
// and SupersedesNodeID for replace_node (the live node the new one replaces). pause_plan and
// resume_plan carry nothing but their op.
type Change struct {
	Op               string
	Node             *Node
	Edge             *Edge
	NodeID           string
	EdgeID           string
	SupersedesNodeID string
}

// Revision is a request to append one revision to a plan.
type Revision struct {
	PlanID           string
	ProjectKey       string
	RequestID        string
	ExpectedParent   int64
	CoordinatorEpoch int64
	AuthorTaskID     string
	Changes          []Change
}

// NodeVersion is a row of the fold: one version of a node. A node whose spec or incoming edges
// change gets a new version; the old one is retired at that revision. RetiredRev is 0 while live.
type NodeVersion struct {
	Node
	SliceDigest      string
	SupersedesNodeID string
	IntroducedRev    int64
	RetiredRev       int64
}

// EdgeRow is a row of the fold. Edges are never edited, only retired.
type EdgeRow struct {
	Edge
	IntroducedRev int64
	RetiredRev    int64
}

// State is the full fold at Revision, history included (live and retired rows): what a revision is
// applied to and what id non-reuse is judged against.
type State struct {
	PlanID     string
	ProjectKey string
	Revision   int64
	Nodes      []NodeVersion
	Edges      []EdgeRow
	// Lifecycle is the history of the plan's and the nodes' lifecycle states (lifecycle.go): not a row of a table, the fold of the lifecycle
	// changes of the log.
	Lifecycle []LifeRow
}

// SnapNode and SnapEdge are the live rows at a revision, as a snapshot carries them.
type SnapNode struct {
	Node
	SliceDigest      string
	SupersedesNodeID string
	IntroducedRev    int64
	// Lifecycle is "" for an active node, else LifePaused, LifeCancelled or LifeArchived. It is no part of the node's slice digest.
	Lifecycle string
}

type SnapEdge struct {
	Edge
	IntroducedRev int64
}

// Snapshot is the plan as of Revision: its live nodes and edges, sorted by id, and the digest of that
// content. Revision 0 is the empty plan.
type Snapshot struct {
	PlanID      string
	ProjectKey  string
	Revision    int64
	Nodes       []SnapNode
	Edges       []SnapEdge
	StateDigest string
	// PlanState is "" while the plan is active, else LifePaused.
	PlanState string
}

// Event is a committed revision: the durable event of the log. Its position (RevisionNo) is the
// cursor a reader resumes from.
type Event struct {
	PlanID           string
	RevisionNo       int64
	ParentRevisionNo int64
	RequestID        string
	RequestDigest    string
	CoordinatorEpoch int64
	AuthorTaskID     string
	RecordedAt       string
	Changes          []Change
	StateDigest      string
}
