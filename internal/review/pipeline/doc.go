// Package pipeline runs independent review over bundle chunks and returns an
// in-memory schema-v1 artifact. Run requires an injected runner and an immutable
// head reader; the command owner supplies agy.Run and a Git-backed reader later.
// No prompt accepts authoring context, custom instructions or repository tools.
// Fixed instructions precede JSON-encoded bundle/finding/code data. These delimit
// data but cannot guarantee a model obeys instructions; output is schema-checked.
//
// Calls, including concurrent Run invocations, are serial and never retried. Each
// reviewer sees each chunk. Documentation-only changes use one reviewer; otherwise
// additions+deletions <=100/400/1000 use 2/3/4 and larger diffs use 5. Thresholds are
// configurable; lenses default to correctness, security, contract/compatibility,
// test validity and correctness again. Renaming code into a document is not docs-only.
//
// Grouping accepts only an exact partition of source indexes, with same-file and
// same-needs-context members. Multi-member groups require every raw member to pass
// deterministic eligibility, preventing an invalid representative erasing valid
// evidence. Support counts distinct reviewer IDs, including across chunks. Group
// failure keeps raw findings unverified and skips verification. Trivial partitions
// skip grouping normally. Semantic overlap is still a model judgement.
//
// Verification reads head lines around each finding, carries its chunk as data and
// never regrades reviewer severity. Failures preserve unverified findings. NeedsContext
// is unverified, stays apart from confirmed and uses the core fail-open confidence
// rule. Final Rules records deterministic drops. ReadLines failure yields partial;
// non-not-exist Lines errors in Rules return an error rather than a valid artifact.
//
// Counts refer to reviewers: any valid normal chunk counts as returned. Invalid or
// unavailable calls are failures, never empty findings. All failed reviewers means
// unavailable with reason and no finding claims; other failed calls/read failures
// mean partial. Calls preserve served model, class/reason, time and token counters.
// Valid output plus cleanup error retains output/class and records Error with partial
// status. Zero result plus host error is unavailable. Cancellation returns no artifact,
// stops further calls and releases the serial guard. Earlier schema-v1 artifacts
// without the additive fields remain valid. Tests use only fake runners/readers.
package pipeline
