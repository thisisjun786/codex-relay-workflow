
## Roles, pairs and aliases

A fresh call names a role with `agentType`. A role has a fixed model and effort (its pair), a role family (`executor` and
`executor-t1` form one, `architect`, `reviewer`, `explorer` and `critic` are alone) and a sandbox:
`workspace-write` for `executor` and `executor-t1`, the only roles that write (to their call's own cwd), `read-only` for
the others. The role table is the helper role store's six pairs, read once per run (see the policy paragraph of the
run options above); a test with no store runs on the fake backend's placeholder pairs and the same alias table.

- With a role key as `agentType`, a `model` option must be absent, the role's model or an alias of its model family
  (`haiku`: `executor` and `explorer`; `sonnet`: `executor-t1`; `opus`: `architect`; `ocx-gpt-6-1-sol`: `reviewer` and
  `critic`); otherwise the call is `refused` with reason `pair_refused`.
- A call without a role key is looked up in the alias table by (a non-role `agentType` string, else the `model` alias;
  `effort`; `isolation`), the most specific matching row deciding: `opus` is `architect`; `haiku` with
  `isolation: 'worktree'` is `executor`; `sonnet` is `executor-t1`; `ocx-gpt-6-1-sol` (as `model` or as `agentType`)
  is `reviewer` at any effort (an `effort: 'xhigh'` is recorded in `requestedPair`, and the thread runs on the reviewer's pair).
  No alias row names `critic`: a critic call names it by `agentType`.
- `haiku` without isolation is ambiguous. Claude runs it in a shared checkout; a CRW call has a write cwd of its own that
  no other call shares, so the call is `refused` with reason `policy_refused`, cause `alias_ambiguous` (the intent row
  and the refusal row carry `cause`), and the detail names both fixes: `isolation: 'worktree'` for a build (role
  `executor`) and `agentType: 'explorer'` for an investigation. Anything else outside the table is `policy_refused` too.
- A fresh call's requested `effort` is never applied and never refused; a differing one is kept in `meta.requestedPair`
  while `meta.configuredPair` is the pair the thread reads back (the role's), for an alias call and for an explicit
  `agentType` call alike.
- A fresh `executor-t1` call needs `stepDecision` (the id `crw.tierStep` returned), otherwise it is `refused` with reason
  `missing_step_decision`.
- The thread's configured pair is read back; a mismatch is `refused` with reason `pair_refused`.
- The run record (the `started` row) keeps `policy_source` and `policy_digest`, and every intent row the digest and the
  call's `sandbox`. A run that resumes a journal under a policy of another digest writes `policy_repinned` (`old`, `new`)
  before its first call; a new thread then takes the new pair, a continuation keeps the pair of its handle.
