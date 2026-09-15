# Working in this repository

- Read [POLICY.md](POLICY.md) for repository CI, merge and release authority and [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution path. `dev` is the default integration branch; `main` accepts only explicitly authorized release promotions from this repository's `dev`.
- This is a personal skill source repository. Read the target skill and its linked references before editing. Keep skill names, folder names, UI prompts, and relative links consistent.
- Maintain the `skills/crw-*` directories together. Shared workflow rules live in `skills/crw-plan/references/integrations.md`; operation-specific rules stay in their owning skill.
- Keep product documents in Linear and private receipts outside this repository. Do not vendor CXC/Paperthin, credentials, session history, or generated runtime state.
- Use a task branch for substantial changes and target `dev`. Preserve dirty work and follow the user's authorization for pushes and publication. Repository gates are `dev-gate` and `release-gate`; checked-in policy does not prove server-side protection is active.
- `scripts/install.py` links this checkout into Codex. It must remain idempotent and refuse to replace existing directories or foreign links. Keep it standard-library-only.
- Validate changed skills with the bundled `skill-creator/scripts/quick_validate.py` when available, check relative links and `git diff --check`, and test installer changes in a temporary destination. Do not use text-matching tests as proof of workflow behavior.
- For instruction changes, reuse valid evidence and run focused scenario review when behavior changes. Live Linear writes and worker launches require scope covering those actions.
- Installed skills may be symlinks to this checkout. Source edits affect subsequent reads immediately; report installation and live execution as separate facts.
