# Luna lane — cheap parallel discovery

A lane of [crw-search](../SKILL.md), not a search skill of its own. It fans out cheap helpers for wide
discovery, then hands every candidate back to the main agent, which runs the `crw-search` proof ladder
(Tier 1 discover, Tier 2 open the source) to settle each claim. Luna discovers; `crw-search` proves; the
main model synthesizes. Never use the lane without this handoff: Luna snippets are leads, not evidence.

Run by a task that may dispatch helpers: [the parent](../../crw-canon/references/hierarchy.md#the-levels) (repository session), a child, or a `child` session (a code id, not a layer)
with helpers of its own. A leaf helper cannot fan out. Trigger phrases: Luna search, `루나검색`, `루나 서치`,
`병렬 웹검색`, `싸게 많이 찾아봐`, cheap or broad web discovery, many source sweeps.

## Spawn path

Dispatch every lane with the `explorer` role key ([execution roles](../../crw-canon/references/execution-roles.md)).
The spawn names the role and carries no model, no reasoning effort and no catalog probe: the explorer's pair
is the helper role store's setting, and the Luna discovery pair is what the operator sets there for this
lane. Fan the lanes out as N spawns before waiting.

The session surface is pinned on its first turn. V1 is the default unless the model catalog selects V2.
If `spawn_agent` is not visible on V1, `tool_search` for it first. V1 `wait_agent` returns final status plus
content, while V2 `wait_agent` is a no-content mailbox. Reuse a lane with V1 `send_input(agent_id)` or V2
`followup_task(task_name)`. V1 also has `close_agent` and `resume_agent`; V2 has only `interrupt_agent`. The
concurrency limits are V1 `agents.max_threads` (default 6) and V2 `max_concurrent_threads_per_session`
(default 4, root included).

These lanes are subagents, not separate Codex tasks: every one runs in this session's working directory on
this branch. Luna lanes are safe to fan out because discovery writes nothing, which is why this shape does not
transfer to parallel write work. Branch or worktree lanes need one task each; see
[Dispatch surfaces](../../crw-pabcd/references/dispatch-surfaces.md).

If the spawn is refused with `unusable helper role settings`, the role store cannot be read for the explorer
role, and the same spawn is refused again against the same store: do not re-issue it and do not probe the
catalog. Report the refusal text as the blocker. The operator repairs it with `crw role helper reset explorer`
(the explorer then inherits the main session model) or by correcting the file, and the lane is dispatched after
that. Until then run the lanes' queries in the main session, one lane at a time, and state plainly which path
each lane took. A role with no override already inherits the main session model on the first spawn and needs
no recovery. There is no silent fallback to another model: the refusal is the signal. Reasoning effort stays
low; final judgment stays in the main session.

## Subagent Skill Attachment

Attach `crw-search` to each lane as [Subagent Skill Attachment (SEARCH-ATTACH-01)](../SKILL.md#subagent-skill-attachment-search-attach-01)
describes. The lane adds only its assignment:

```text
message: "[$crw-search](skill://<crw-search SKILL.md absolute path>)
TASK: one lane in a Luna search swarm. LANE: <source class / query family>. Run 5-10 distinct queries; open the source for every result that matters. Return 3-5 findings with URLs, dates, source type, primary-or-lead flag. No edits, no questions."
```

## When to use it

Use the lane when breadth matters and each subtask can be narrow:

- release, news and changelog sweeps across many vendors
- competitor or ecosystem scans
- "find many sources first, judge later" research
- workflows where the main session conserves quota by delegating source discovery

Do not use it for repository grep, a one-source latest or current fact, implementation work, or high-stakes
final advice without primary-source proof.

## Swarm shape

Default to five `explorer` helpers. Use three for smaller research and two for corroboration only.

Assign one distinct lane per helper. Do not send duplicate prompts: rewrite the user request into query
families, then give each helper one family:

- official docs and changelogs (`site:<docs-domain> changelog`)
- vendor blogs and release notes (`site:<blog-domain> "release notes"`)
- GitHub releases, issues and discussions (`site:github.com <topic>`)
- standards, specs and API references (`filetype:pdf <topic> spec` or `intitle:specification <topic>`)
- independent reports, benchmarks and community findings
  (`site:reddit.com OR site:news.ycombinator.com <topic> after:<date>`)

Search English first, the largest authoritative corpus. Add a local-language sweep only when the topic is
inherently local or the user asks for sources in a specific language.

Each helper gets the `crw-search` mention and a short task naming its lane; the skill carries the tool list and
the proof rules. Spawn all lanes in one turn, in parallel. Report the spawned agent ids or nicknames to the
user; the runtime may choose nicknames, so do not claim manual naming unless the spawn tool supports it.

## Proof handoff

Luna output is candidate evidence only. After the swarm returns, the main agent runs the `crw-search` proof
ladder on the strongest candidates:

1. Build a compact claim ledger: claim, source URL, date, source type, Luna lane, status. Status is
   `candidate`, `verified`, `contradicted` or `unreachable`.
2. Open primary sources (Tier 2) before final synthesis. Prefer official docs, release notes, source
   repositories, specs and original announcements.
3. When sources conflict, state which source wins and why. Do not average.
4. Mark snippet-only or unreachable items as unverified leads.

A high-risk non-code claim (price, market share, a date, a cause) needs at least two independent source domains
and a counter-search before it is promoted to verified.

## Final report

1. Spawn path: the `explorer` role used, or serial fallback after a refusal.
2. Swarm: the number of helpers and their lanes.
3. Verified findings: source-opened claims only.
4. Open leads: promising but unverified Luna results.

The lane runs no expansion loop, keeps no session journal, executes no code to verify and writes no report;
those are [Tier 3](deep-research.md). Use the lane when breadth for cost is the goal and escalate to Tier 3
when the question needs iterative expansion and contested-claim verification.
