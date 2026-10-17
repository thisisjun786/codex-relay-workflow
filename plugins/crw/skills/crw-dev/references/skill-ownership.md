# Skill Ownership Map

Each rule area has exactly one canonical owner. Other skills may contain stubs but MUST NOT duplicate canonical content.

| Rule Area | Canonical Owner | Stub Locations |
|-----------|----------------|----------------|
| Circular dependencies | `dev-architecture` | `dev`, `dev-code-reviewer` |
| Module boundaries / layers | `dev-architecture` | `dev-backend`, `dev-frontend` |
| Coupling taxonomy | `dev-architecture` | `dev-code-reviewer` |
| Barrel / re-export | `dev-architecture` | `dev-scaffolding` |
| Pre-write search | dev references/development-practice.md (§1.5) | `dev-code-reviewer` |
| Independent peer collaboration | `dev/references/peer-collaboration.md` | `dev`, `search`, `crw-work`, `crw-continue`, structure 20/60 |
| Native execution selection / tool composition | `dev/references/native-execution.md` | `dev`, `crw-continue`, `crw-work`, peer collaboration, structure 60 |
| Mid-work async user questions | `dev/references/async-questions.md` | `dev` §0, `crw-continue`, structure 60; questions before registration are `crw-define`'s |
| Stacked pull requests (`DEV-STACK-*`) | `dev` `references/stacked-prs.md` | `crw-work`, `crw-continue`, `dev-code-reviewer`, `dev-devops` |
| Edge-first testing | `dev-testing` §6 | — |
| Flaky tests / CI re-run (`TEST-FLAKE-*`) | `dev-testing` `references/ci-pipeline.md` §5 | `dev-testing` §5.4, `dev-debugging` Scenario D, `dev-devops` `references/ci-cd-deploy.md` §6.2/§6.5 |
| Manual surface QA / evidence matrix | `crw-qa` | `dev-testing` §4.7; selection in `dev/references/browser-routing.md` |
| Test-induced defense | `dev-testing` §6.7 | `dev-code-reviewer` |
| Boundary-only defense | `dev-architecture` §4 | `dev-backend`, `dev-security` |
| Process isolation | `dev-backend` references/ | `dev-code-reviewer`, `dev-devops` |
| Long-lived connections | `dev-backend` §1 app hooks | `dev-frontend`, `dev-devops` operational gates |
| Async task queue | `dev-backend` §2 app hooks | `dev-devops` operational gates |
| Debugging methodology | `dev-debugging` | `dev-code-reviewer` |
| Browse / QA tool routing | `dev/references/browser-routing.md` | `dev`, `dev-testing` §4.7, `search`, `dev-frontend`, `dev-visualizer` |
| Data pipeline patterns | `dev-data` | `dev-backend` |
| Frontend implementation | `dev-frontend` | `dev-uiux-design` |
| Design intent discovery | `dev-uiux-design` | `dev-frontend` |
| Design judgment | `dev-uiux-design` | `dev-frontend` |
| Visual document composition / diagram and report rendering and delivery mechanics | `dev-visualizer` | `dev`; format-specific document owners retain PDF/DOCX/Slides mechanics |
| Reader-facing document structure (READER-DOC-*) | `dev/references/reader-documents.md` | `crw-canon` Family invariants, `dev-visualizer`, `crw-work` plan-output/phase-check/D/implementation-units, `kwrite`, `search` deep-research |
| Deep research protocol (SEARCH-DEEP-*) | `search/references/deep-research.md` | `search` Tier 3, `dev` browser-routing |
| Operational gates | `dev-devops` | `dev-backend`, `dev-scaffolding` |
| Repository bootstrap (rulesets, merge settings, PR limits) | `crw-canon` `references/repo-bootstrap.md` | `crw-canon` `references/repo-governance.md`, `dev-devops` |
| Agent PR intake and supersede (`DEVOPS-AGENT-*`, `DEVOPS-PR-SUPERSEDE-01`) | `crw-canon` `references/agent-pr-intake.md` | `crw-canon` `references/repo-governance.md`, `dev` stacked-prs, `dev-devops` agent-infra-safety, ci-cd-deploy |
| Local worktree/branch GC (`DEVOPS-LOCAL-GC-*`) | `dev-devops` `references/local-gc.md` | `worktree-guardian` §4, `dev-devops` ci-cd-deploy |
| Native desktop acceptance matrix (`DESKTOP-*`) | `dev-devops` `references/native-desktop-acceptance.md` | `dev` routing table, `dev-testing`, `qa`, `cross-platform-release.md`, `mobile-native.md`, `dev-debugging` runtimes/swift.md |
| macOS system approval prompts (`MACOS-APPROVAL-*`) | `dev-devops` `references/macos-system-approvals.md` | `cross-platform-release.md` §2/§3, `qa` Desktop GUI row |
| Project scaffolding / docs | `dev-scaffolding` | `crw-work` |
| C0/C1 classification and record exemption | dev §0.0/§0.1 | crw-work, dev-scaffolding |
| Unit residence and numbered roadmap contents | crw-work references/implementation-units.md, subject to dev §0.1 | crw-continue, dev-scaffolding |
| Loop intent, docs-first activation, scoped continuation | crw-continue SKILL.md | crw-work, goalplan |
| Goalplan schema and runtime lifecycle | crw-continue references/durable-goalplan.md and references/runtime-lifecycle.md respectively | crw-work, goalplan |
| Phase control and phase work | crw-work references/phase-control.md and phase-plan/audit/check.md respectively | crw-continue, orchestrate |
| C2+ P-phase loop-spec output fields | crw-work references/plan-output.md | crw-work P router, phase-plan |
| Repair and optimization method | crw-work references/loop-engineering.md and optimization.md respectively | crw-continue |
| task work workflow | `crw-work` | — |
| Anti-slop output | `crw-canon` `references/skill-family.md` (Family invariants) | all `dev-*` |
| file:line evidence | `crw-canon` `references/skill-family.md` (Family invariants) | all `dev-*` |
| Completion proof | `dev` §3 verification gate; `crw-canon` `references/skill-family.md` (Family invariants) | `crw-work`, all `dev-*` |

When updating a rule, update the canonical owner first, then verify stubs still point correctly.
