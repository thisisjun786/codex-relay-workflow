// Package bundle builds independent review prompt material from committed Git
// objects. Build is the only entry point; inputs are repo, base, head and numeric
// Options. Author conversations, packets, issues and PR bodies are not inputs.
// No content is executed, evaluated or followed. This package runs no reviewer.
//
// The diff is merge-base(base, head)..head, using one unique merge base. Context
// defaults to 25 lines (zero means default). Head contents use git show head:path;
// working files and uncommitted attributes are ignored. Git 2.43+ is required for
// head-sourced attributes. External diff/textconv and replace objects are disabled.
// Determinism is for the same Git version, objects and options; there are no dates.
//
// MaxFileBytes defaults to 64 KiB of numbered head payload, MaxRuleBytes to 32 KiB
// of numbered rule payload divided evenly among AGENTS.md, POLICY.md and
// CONTRIBUTING.md, and MaxChunkBytes to 256 KiB including all framing and repeated
// rules. Missing rules are recorded. Zero caps mean defaults, negatives are errors.
// Oversized head text keeps new-side hunk ranges in line order up to the cap,
// retaining a valid UTF-8 prefix of a final long line. A no-hunk rename keeps its
// first Context lines. Truncation is marked in text and metadata. Diffs remain
// complete at configured context. An indivisible group or preamble over the chunk
// cap returns ErrSizeLimit, never partial material disguised as a complete bundle.
// These are output caps; Git outputs are collected before rendering, so peak
// input memory is not bounded by these options. The caller's context cancels Git.
//
// Changes sort by head path (old path for deletions), then fill contiguous chunks
// greedily. Each chunk repeats identities, statistics, rule excerpts and the data
// header; its Paths and Text pair each file's diff with that file's head material.
// Deleted files have no head text, binary/invalid-UTF-8 files are named without
// inlining head bytes, renames retain both names and full head text, symlinks show
// their target blob without following it, and gitlinks are named without file text.
// Type-change deletion/addition patches stay in one group. All data lines carry a
// prefix and paths are quoted, separating embedded marker-like text from framing.
// This framing is not a guarantee against a reviewer following hostile content.
//
// Metadata records commit IDs, merge base, stable patch-id from a separate U3
// full-range diff (empty when no changes), diff counts, exact payload/chunk sizes,
// file line counts and truncation. The pipeline can map these to its artifact and
// feed one Chunk.Text per reviewer call; no artifact/schema or model is imported.
package bundle
