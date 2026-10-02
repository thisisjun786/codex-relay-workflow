// Package dagsched is the DAG scheduler: it reads a stored plan (internal/relay/dag, CRW-183) and the
// relay's execution records and decides what may run next, releases it, and records what the parent
// accepted and what landed. It is the code behind the relay commands dag-ready, dag-release, dag-accept,
// dag-integration-observe, dag-decision-record, dag-correct, dag-region-declare, dag-conflict-observe,
// dag-merge-judge, dag-merge-request and dag-cap-basis-record (docs/relay/dag-scheduler.md).
//
// The scheduler is a set of deterministic functions over the relay store and the declared artifact bytes. It
// holds no goal, runs no loop, starts no daemon and opens no database of its own: a goal-free parent calls it
// when a relay result, a block or a decision request wakes it, and ends its turn when only waiting remains.
// No predicate here depends on a model's judgement (contract 0 and 2.5). A node's value is an acceptance
// record bound to its consumed input manifest, its criteria digest and its verification evidence; a child's
// completion, a transport acknowledgement and a statement in a report open no edge by themselves.
package dagsched

