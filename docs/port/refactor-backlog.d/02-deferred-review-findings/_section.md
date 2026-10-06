## Deferred review findings

The backlog pass of 2026-09-30 (PRs #199 to #204) was the first edit of this section's existing
bullets: each keeps its tag and text and gains a status clause, and the list is split into what
is still open and what the pass closed. The cleanup CRW-247 (2026-10-01, checked against dev
03724ea7) was the second: it moved the entries that refactors R1 to R3 and CRW-242 to CRW-246
closed, or whose target code they deleted, to a second Closed list, each with one status clause
naming the PR, and left every other entry as it was. The cleanup CRW-252 (2026-10-02, checked
against dev d3a93454) was the third: it moved the five entries that CRW-248 to CRW-251 (project
P-CRW-114, the Go runtime's operations defects) closed to a third Closed list, each with one status
clause naming the issue, the PR and its merge, and appended the two review findings of PRs #261 and
#266 that those PRs left open to Still deferred.
The record of the real-use run 3 (CRW-257, 2026-10-02, checked against dev 3a71c5d5) was the fourth.
It read every review, review thread and comment of PRs #271 to #276 (CRW-253 to CRW-256, CRW-258 and
CRW-259, project P-CRW-115) to the end of pagination and appended what those PRs and their bodies
leave open to a new list at the end of Still deferred, "Real-use run 3 (P-CRW-115)"; it recorded in a
fourth Closed list the one entry that the project itself closed and gave the PR #269 informational
entry its status clause. Devin raised five findings on those PRs (three red, two yellow, five
threads, all of which got a reply and were resolved) and none on #272, #273 and #275, and the Codex
review summary comments show the security review completed and name no finding. The three red
findings were fixed in their own PRs and are not deferred work: the red of #271 in 7e191b34 and
67b97786, the red of #274 in 7e022333, the red of #276 in 56997c3b and ea2f28e4. The two yellow
findings were narrowed there, not closed (the yellow of #271 in 7e191b34, the yellow of #276 in
56997c3b), and what each left is an entry.
The record of the real-use run 4 (CRW-272, 2026-10-02, checked against dev aca33217) was the fifth.
It read every review, review thread and comment of PRs #278 to #286 and #288 to #290 (CRW-260 to CRW-271,
project P-CRW-116, twelve PRs; #287 belongs to another project) to the end of pagination, through the GitHub API and the
relay's merge-evidence. Devin raised five findings on those PRs (two yellow, three analysis, in five threads, all of
which got a reply and were resolved) and none on #278, #279, #283, #284, #285, #288, #289 and #290, and the Codex
review summary comment on each PR shows the security review completed and names no finding. No finding was red. The two
yellow findings were answered without a code change and are entries (#280, comment 4162808623; #286, comment
4163002737); of the three analysis findings, two were fixed in their own PRs (#281 in 1af7f126, #282 in 5d1370a7) and
the third, on #282, asked for the closure this record makes (comment 4162830060). The record moved the sixteen entries
that CRW-260 to CRW-271 closed to a fifth Closed list, each with one status clause naming the issue, the PR and its
merge, gave the three it closed only in part a status clause for what is done and what is left, and appended what the
PRs and their bodies leave open to a new list at the end of Still deferred, "Real-use run 4 (P-CRW-116)".
File and line references inside an original bullet
are as of dev 6da55d72 (the CRW-247 clauses as of dev 03724ea7, the CRW-252 clauses as of dev
d3a93454, the CRW-257 entries and clauses as of dev 3a71c5d5, the CRW-272 entries and clauses as of dev aca33217). New findings are
still appended under Still deferred in the entry format `tag - defect - Go: path - Python: path -
deferred: reason`, followed by a status clause.
