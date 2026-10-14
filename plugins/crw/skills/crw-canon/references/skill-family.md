# Skill family

The skills of the plugin by the job they do and the level that calls them, and how a skill is named under each installation. A skill that names another links to its directory; this file is the one place that says which skills exist together.

| Family | Skills | Called by |
|---|---|---|
| Define and plan | `crw-define`, `crw-plan`, `crw-add-issue` | `crw-define` by the top session (today Sion) or the parent (repository session) when a milestone and its issues are registered; `crw-plan` and `crw-add-issue` by the initiative lead, the manager, or the user; `crw-add-issue` also by the project orchestrator |
| Run | `crw-run`, `crw-run-initiative` | a project orchestrator (`crw-run`) or an initiative lead (`crw-run-initiative`), at the level its task is bound to |
| Report and check | `crw-status`, `crw-check`, `crw-tidy`, `crw-refactor` | any level, read-only unless the skill says what it writes |
| One task | `crw-loop`, `crw-pabcd` | an issue child, or a task without a parent |
| Development surfaces | `crw-dev`, the `crw-dev-*` skills, `crw-qa` | the task that edits the code |
| Tools | `crw-search`, `crw-recall`, `crw-repo-map`, `crw-ast-grep`, `crw-kwrite`, `crw-worktree-guardian` | any task that needs the tool |
| Shared references | `crw-canon` | never called: it is read through the links of the other skills |

## Skill names under each installation

These documents name the skills without a prefix, which is how a linked
installation exposes them. A plugin installation namespaces every skill under the
plugin, so `crw-run` is offered to the model as `crw:crw-run` and the same mapping
applies to each name above. Read a name written here as whichever spelling the
current installation exposes, and use the exposed spelling when invoking a skill
or telling a user how to invoke one. The directory name, the file layout, and the
relative links between these documents are identical in both installations.

## Family invariants

> **Role boundary (canonical; `crw-dev-frontend` and `crw-dev-uiux-design` carry the same paragraph):**
> `crw-dev` owns universal process, evidence, and safety rules. `crw-dev-uiux-design` owns
> design intent, direction, and concept judgment. `crw-dev-frontend` owns concrete frontend
> implementation and rendered tell enforcement. Anti-slop has three layers: `crw-dev` =
> output/process hygiene (FAMILY-SLOP-01), `crw-dev-uiux-design` = concept/taste judgment
> (is this direction generic or domain-wrong?), `crw-dev-frontend` = rendered implementation
> tell detection and removal (FE-AI-TELL-01).

These hold for every `crw-*` skill and every response they govern. This file is their one
definition; `crw-dev` and the other skills link here and do not restate them. They are
agent-followed wording, not runtime gates.

- **Anti-slop output (FAMILY-SLOP-01).** No filler, no performative narration, no decorative
  rationale. Ship no placeholders, TODO-only deliverables, fake fallbacks, speculative wrapper
  layers, or broad defensive clutter without a named boundary reason. The code-smell catalog is
  in `crw-dev-code-reviewer` §3; this rule is about not emitting slop in the first place.
  Prose and document deliverables have their own reflexes (DEFAULT): em dashes and
  connector openers in Korean text, bold-label bullets and rule-of-three lists where
  a sentence would do, and stat cards, tinted callouts and box-and-arrow figures in a
  printed page. `crw-kwrite` owns the Korean sentence tells; `crw-dev-visualizer`
  REPORT-DESIGN-01 owns the page-design tells.
- **Reader deliverables (FAMILY-READER-01).** A report, explainer, visual document or summary
  written for a person follows [Reader documents](../../crw-dev/references/reader-documents.md): answer
  first, evidence separated and anchored, fresh-reader check for delivered reports. Audit
  artifacts (receipts, logs, ledgers) keep their raw form and are linked, not narrated.
- **file:line evidence (FAMILY-CITE-01).** When reporting code findings, plans, reviews, or
  contradictions, cite `path:line`. Plans list exact paths and the verification command; review
  and audit findings carry `path:line`; verification claims carry the command and its output or
  artifact path.
- **Completion proof (FAMILY-PROOF-01).** No completion claim without fresh proof; the long form
  is the verification gate in `crw-dev` §3. Every other skill inherits that gate and does not
  restate it.
