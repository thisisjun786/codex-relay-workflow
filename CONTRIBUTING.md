# Contributing

Read [POLICY.md](POLICY.md) for branches, review and release rules and
[AGENTS.md](AGENTS.md) for source boundaries. Installation and skill usage are
in [README.md](README.md).

## Make a change

Start from `dev` on a short-lived branch. Use a separate worktree when another
task owns the checkout, and preserve its uncommitted work. Read the target skill
and linked references before editing. Shared workflow rules belong in
`skills/linear-plan/references/integrations.md`; operation-specific guidance
belongs with that skill.

Keep each PR focused on one outcome. Explain the triggering problem, expected
behavior and acceptance example in the PR even when there is a Linear link.
Access to the maintainer's private project is not a contribution prerequisite.
Korean and English contributions are welcome. Use only material you have the
right to contribute, preserve imported notices, and omit private task records
and credentials. Contributions are provided under this repository's
[MIT license](LICENSE). External dependencies retain their own licenses.

For bugs, open a GitHub issue with the source commit, relevant skill, host version,
expected behavior, and a minimal redacted reproduction. Use [SECURITY.md](SECURITY.md)
for vulnerabilities. Private Linear links may provide context, but keep enough
public detail in the issue or PR for contributors to understand the change.
Do not copy private Linear descriptions, attachments, or task transcripts into
public reports. Check integration settings before linking: a bot linkback may
copy an entire issue description, not just its URL.

## Check locally

Use Python 3.10 or newer. No dependency installation is needed for these checks:

```sh
python3 scripts/ci/validate.py
python3 -m unittest discover -s scripts/ci/tests -v
git diff --check
```

Installer tests use temporary destinations; do not point test runs at your real
Codex skill directory. The bundled Codex skill validator, when installed, is an
additional check described in the README, not a hosted CI dependency.

Instruction changes need realistic scenario review as well as structural
validation. Reuse existing evidence for unchanged bytes and criteria. Keep live
Linear writes, worker creation, hook registration and service changes within
their separately authorized task scope. See [CI operation](docs/CI.md) for
scanner setup and the difference between offline checks and live proof.

## Publish for review

Open ordinary PRs against `dev`. When basic checks pass and the change can be
reviewed, open it Ready for review or mark the draft ready, then request review.
Handle findings, fixes and replies on that same PR, keeping it ready during
normal corrections. Include exact checks and their results, and identify
untested behavior. An absent check or review is not a pass.

The implementation owner handles reviews through resolution; the coordinator
checks the latest candidate before integration. Every promotion from `dev` to
`main` needs explicit owner release authorization and release notes. A normal
contribution does not publish or deploy anything.
