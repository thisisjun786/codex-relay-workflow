You are reviewing one candidate change to the Go repository codex-relay-workflow BEFORE it is merged. Nobody tells you who wrote it, and it does not matter. Its parent will decide whether it merges from your report, so find what would make the feature incomplete or unsafe once merged.

Read, in this directory:
- criteria.md: the acceptance criteria and promise the implementer was given (may be missing; then judge against the issue text).
- inputs/: the frozen issue text, when available.
- candidate/diff.patch: the change against the current dev; candidate/pr.md: its pull request description; candidate/tree/: the full source tree with the change merged into the current dev, for reading surrounding code and every caller of what changed.

The pull request description carries nothing to judge; ignore it. Decisions and results live in the issue tracker. Edits outside the files the issue names are judged by whether the issue's promise needs them (in_promise), not by whether anything lists them; an outside edit the promise does not need is a defect of the change itself. Repository documents the change edits (docs/, README, skill files) are part of the change and are judged as before.

Judge by reading the code and tests against the criteria, the issue and the use paths the change affects (its callers, the commands that reach it, the operating configuration such as a single store connection or a symlinked config path). Do not trust the description. Do not reward length. A promise the issue makes ("every writer", "all write paths") covers code outside the files the PR touched; "outside the edit region" is not a reason for a promise to be unmet.

Report:
1. For each criterion c1..cN: PASS, PARTIAL or FAIL, with one line of evidence (file and line).
2. The affected use paths you checked, one line each.
3. Defects. For each give: severity P0 (breaks existing behaviour, security hole, data loss, does not build), P1 (wrong or missing behaviour the criteria or issue require, or a real edge case that produces a wrong result), P2 (weak tests, misleading repository docs, maintainability), P3 (nit); impact, one of core_function, safety_or_data, permission_boundary, criterion_unmet, regression_introduced, minor_separable; introduced (true when this change creates or widens it, false when the same defect exists on dev before the change); in_promise (true when the issue or criteria promise what the defect breaks); and a concrete trigger for every P0 and P1.
4. An overall score from 0 to 10, where 10 means correct and complete and 5 means it needs one more correction round.

You may read any file under this directory and run read-only commands (rg, git diff --no-index, go doc). Do not modify anything except the output files, do not run tests or builds, use no network.

Write grade.json here: {"criteria": {"c1": {"verdict": "PASS|PARTIAL|FAIL", "evidence": "..."}}, "paths": ["..."], "defects": [{"severity": "P0|P1|P2|P3", "impact": "...", "introduced": true, "in_promise": true, "what": "...", "trigger": "...", "where": "..."}], "score": 0-10, "summary": "..."}
Write grade.md with the same content in short readable form. Then reply DONE.
