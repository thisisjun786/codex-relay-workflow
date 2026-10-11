// Package trace is the crw-flow-trace/1 contract: the offline record of what a flow run did, written from a copy of a relay
// store and read back by the shadow comparator and the measurement reporter (internal/flow/shadow). It carries the journal
// rows of the run in order (the event ordinals), the stage transitions of every node, the calls with their tokens, the
// decisions the run recorded, and the delivery, acceptance and integration times the relay stored. Export reads the flow_*
// and dag_* rows and the delivery rows of a store copy and writes nothing to it; the reader is strict (an unknown member, a
// wrong schema or a row out of order is an error), so a trace that loads is a trace every tool reads the same way.
//
// The package reads a store copy and never a live store: RefuseLive is the first thing Export, the shadow command and the
// measure command do with a path they were given.
package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// Schema is the format name a trace file carries.
const Schema = "crw-flow-trace/1"

// Trace is one trace file.
type Trace struct {
	Schema      string       `json:"schema"`
	Source      Source       `json:"source"`
	Runs        []Run        `json:"runs"`
	Deliveries  []Delivery   `json:"deliveries"`
	Acceptances []Acceptance `json:"acceptances"`
	Integration []Batch      `json:"integration"`
	Aux         Aux          `json:"aux"`
}

// Source says which store copy the trace was written from: the SHA-256 of the copy's file (the same before and after the
// export), its size and the store id its schema_meta names. The store id is the id of the store the copy was made from, so
// it is not an identity the live refusal can use (a faithful copy has the live store's id); the refusal uses the file's own
// identity and path (RefuseLive).
type Source struct {
	Digest  string `json:"digest"`
	Bytes   int64  `json:"bytes"`
	StoreID string `json:"storeId"`
}

// Run is one flow run of the copy.
type Run struct {
	RunID        string     `json:"runId"`
	Workflow     string     `json:"workflow"`
	ScriptDigest string     `json:"scriptDigest"`
	ArgsDigest   string     `json:"argsDigest"`
	PlanID       string     `json:"planId"`
	PlanRevision int        `json:"planRevision"`
	ResumeFrom   string     `json:"resumeFrom"`
	StartedAt    string     `json:"startedAt"`
	Events       []Event    `json:"events"`
	Nodes        []Node     `json:"nodes"`
	Calls        []Call     `json:"calls"`
	Decisions    []Decision `json:"decisions"`
}

// Event is one journal row of a run: a flow_calls row or a flow_events row, in the order the journal appended them (Seq). It
// is the engine's own Event with the time the row was recorded.
type Event struct {
	Seq      int            `json:"seq"`
	Kind     string         `json:"kind"`
	Event    string         `json:"event"`
	CallID   string         `json:"callId,omitempty"`
	Path     string         `json:"path,omitempty"`
	Position int            `json:"position,omitempty"`
	Op       string         `json:"op,omitempty"`
	Label    string         `json:"label,omitempty"`
	At       string         `json:"at"`
	Data     map[string]any `json:"data,omitempty"`
}

// Engine is the row as the engine's journal holds it.
func (e Event) Engine(runID string) engine.Event {
	return engine.Event{Seq: e.Seq, RunID: runID, Kind: e.Kind, CallID: e.CallID, Path: e.Path, Position: e.Position, Op: e.Op, Label: e.Label, Event: e.Event, Data: e.Data}
}

// Node is the stage transitions of one node of the run, oldest first. A transition names the stage the node entered (build,
// review, final_review, plan and explore when a call of that role started; released, accepted, integrated and the closing
// states from the dag_agent_* rows), when, and the journal row or the store row it was read from.
type Node struct {
	NodeID      string       `json:"nodeId"`
	Transitions []Transition `json:"transitions"`
}

// Transition is one stage change of a node.
type Transition struct {
	Stage  string `json:"stage"`
	At     string `json:"at"`
	Seq    int    `json:"seq,omitempty"`
	CallID string `json:"callId,omitempty"`
	Source string `json:"source"`
	Detail string `json:"detail,omitempty"`
}

// Tokens is the token usage a call recorded.
type Tokens struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Total  int64 `json:"total"`
}

// Call is one agent call of the run, restored from its flow_calls rows only (engine.RestoreCallResult). A call with no
// terminal row has Status "open".
type Call struct {
	CallID           string `json:"callId"`
	Path             string `json:"path"`
	Position         int    `json:"position"`
	Label            string `json:"label,omitempty"`
	Role             string `json:"role"`
	Family           string `json:"family,omitempty"`
	Node             string `json:"node,omitempty"`
	Model            string `json:"model,omitempty"`
	Effort           string `json:"effort,omitempty"`
	ResumeOf         string `json:"resumeOf,omitempty"`
	StepDecision     string `json:"stepDecision,omitempty"`
	SchemaDigest     string `json:"schemaDigest,omitempty"`
	ThreadID         string `json:"threadId,omitempty"`
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	Tokens           Tokens `json:"tokens"`
	TokensUnmeasured bool   `json:"tokensUnmeasured,omitempty"`
	RepairUsed       bool   `json:"repairUsed,omitempty"`
	Turns            int    `json:"turns"`
	PreparedSeq      int    `json:"preparedSeq"`
	PreparedAt       string `json:"preparedAt"`
	TerminalSeq      int    `json:"terminalSeq,omitempty"`
	TerminalAt       string `json:"terminalAt,omitempty"`
	// Result is the last agent message of a completed call, the answer a replay gives the script for it.
	Result string `json:"result,omitempty"`
}

// Decision is a decision the run (or its parent) recorded outside the call rows: a flow_spawn_decisions row, a
// flow_step_decisions row, or the escalation a dag_agent_closures row records.
type Decision struct {
	Source          string `json:"source"`
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Node            string `json:"node,omitempty"`
	CallID          string `json:"callId,omitempty"`
	Rule            string `json:"rule,omitempty"`
	Decision        string `json:"decision,omitempty"`
	Reason          string `json:"reason,omitempty"`
	RequestedModel  string `json:"requestedModel,omitempty"`
	RequestedEffort string `json:"requestedEffort,omitempty"`
	AppliedModel    string `json:"appliedModel,omitempty"`
	AppliedEffort   string `json:"appliedEffort,omitempty"`
	FromTier        string `json:"fromTier,omitempty"`
	ToTier          string `json:"toTier,omitempty"`
	At              string `json:"at"`
}

// Delivery is the relay's delivery of one event: when it was queued, when its first counted send started, how many sends and
// busy answers it took. A send is an attempt that is not a withheld or busy-deferred one (store.RelationshipSendsSQL).
type Delivery struct {
	EventID         string `json:"eventId"`
	RelationshipID  string `json:"relationshipId"`
	RecipientTaskID string `json:"recipientTaskId"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	HoldReason      string `json:"holdReason,omitempty"`
	CreatedAt       string `json:"createdAt"`
	FirstSentAt     string `json:"firstSentAt,omitempty"`
	Sends           int    `json:"sends"`
	BusyAnswers     int    `json:"busyAnswers"`
}

// Acceptance is one acceptance of a node: an agent acceptance (dag_agent_acceptances, with the reviewer call it stands on) or
// a relationship acceptance (dag_acceptances).
type Acceptance struct {
	AcceptanceID   string `json:"acceptanceId"`
	Kind           string `json:"kind"`
	PlanID         string `json:"planId"`
	NodeID         string `json:"nodeId"`
	ExecutionID    string `json:"executionId,omitempty"`
	ReviewerCallID string `json:"reviewerCallId,omitempty"`
	At             string `json:"at"`
}

// Batch is one integration batch: when its first stage row was written, when it moved the target (MovedAt, empty if it did
// not), and its members. A batch that moved nothing and marked nothing did not land.
type Batch struct {
	BatchID  string      `json:"batchId"`
	PlanID   string      `json:"planId"`
	Ref      string      `json:"ref,omitempty"`
	IntentAt string      `json:"intentAt"`
	MovedAt  string      `json:"movedAt,omitempty"`
	Landed   bool        `json:"landed"`
	Nodes    []BatchNode `json:"nodes"`
}

// BatchNode is one member of a batch: when its intent row was frozen, when it was marked integrated.
type BatchNode struct {
	NodeID       string `json:"nodeId"`
	AcceptanceID string `json:"acceptanceId,omitempty"`
	IntentAt     string `json:"intentAt"`
	MarkedAt     string `json:"markedAt,omitempty"`
	MarkPending  bool   `json:"markPending,omitempty"`
}

// Aux are the counts of the tables the R11 and R12 decisions look at.
type Aux struct {
	SyncOutbox  SyncOutbox  `json:"syncOutbox"`
	MergeTrains MergeTrains `json:"mergeTrains"`
}

// SyncOutbox counts the sync_outbox rows by state.
type SyncOutbox struct {
	Rows    int            `json:"rows"`
	ByState map[string]int `json:"byState"`
}

// MergeTrains counts the merge-train rows.
type MergeTrains struct {
	Trains  int `json:"trains"`
	Members int `json:"members"`
	Events  int `json:"events"`
}

// Marshal is the bytes of a trace file: indented JSON with a final newline, the same bytes for the same trace.
func Marshal(t *Trace) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(t); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Write stores a trace at path. The file is written whole under a temporary name beside it and renamed, so a reader never
// sees half a trace. A path that is a live store (RefuseLive) is refused, and so is a path that already is a directory.
func Write(path string, t *Trace) error {
	if err := RefuseLive(path); err != nil {
		return err
	}
	data, err := Marshal(t)
	if err != nil {
		return err
	}
	return WriteBytes(path, data)
}

// WriteBytes writes data at path as Write does, for the reports of the tools that read a trace.
func WriteBytes(path string, data []byte) error {
	if err := RefuseLive(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".crw-flow-out-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// ReadFile reads and validates the trace at path. A path that is a live store is refused before anything is read.
func ReadFile(path string) (*Trace, error) {
	if err := RefuseLive(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// Parse reads a trace strictly: an unknown member, trailing data, another schema or a run whose rows are out of order is an
// error.
func Parse(data []byte) (*Trace, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var t Trace
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("not a %s file: %w", Schema, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("not a %s file: data after the trace", Schema)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Validate checks what a reader relies on: the schema, one run record per run id, the journal rows of a run in strictly
// increasing order, every call and decision naming a row of its run.
func (t *Trace) Validate() error {
	if t.Schema != Schema {
		return fmt.Errorf("trace schema is %q, want %q", t.Schema, Schema)
	}
	seen := map[string]bool{}
	for i := range t.Runs {
		r := &t.Runs[i]
		if r.RunID == "" {
			return fmt.Errorf("run %d has no run id", i)
		}
		if seen[r.RunID] {
			return fmt.Errorf("run %s appears twice", r.RunID)
		}
		seen[r.RunID] = true
		last := 0
		rows := map[int]bool{}
		for _, e := range r.Events {
			if e.Seq <= last {
				return fmt.Errorf("run %s: journal row %d follows row %d: rows are in strictly increasing order", r.RunID, e.Seq, last)
			}
			last = e.Seq
			rows[e.Seq] = true
		}
		for _, c := range r.Calls {
			if c.CallID == "" || !rows[c.PreparedSeq] {
				return fmt.Errorf("run %s: call %q names no prepared row", r.RunID, c.CallID)
			}
		}
	}
	return nil
}

// Run is the run with id, nil when the trace has none.
func (t *Trace) Run(id string) *Run {
	for i := range t.Runs {
		if t.Runs[i].RunID == id {
			return &t.Runs[i]
		}
	}
	return nil
}

// Select returns the trace with only the run id kept (all runs for ""). It is an error to name a run the trace lacks.
func (t *Trace) Select(id string) (*Trace, error) {
	if id == "" {
		return t, nil
	}
	r := t.Run(id)
	if r == nil {
		return nil, fmt.Errorf("the trace holds no run %q", id)
	}
	out := *t
	out.Runs = []Run{*r}
	return &out, nil
}
