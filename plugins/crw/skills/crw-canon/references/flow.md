# The crw flow commands

`crw flow` is the command line of the flow engine: the process that runs a workflow script for one run, and the commands that start, show, follow, answer, pause, cancel, resume and recover a run. This page is the one reference of the commands; the script format is [docs/flow/script.md](../../../../../docs/flow/script.md), the journal a run writes is [docs/flow/journal.md](../../../../../docs/flow/journal.md), and the scheduler a run asks for its slots is [docs/relay/dag-scheduler.md](../../../../../docs/relay/dag-scheduler.md).

A run is a process that lives for one run and loses nothing when it dies: every fact is a row of its journal in the relay store, the run lock (`<state>/flow/<run>/run.lock`) says whether a process holds the run, and `crw flow resume` or `crw flow recover` takes a run whose lock is free from the journal again. The orchestrator thread that started a run owns it; the Stop hook prints the run's status line in that thread only, at the end of its turns. While the thread is idle, the progress is read with `crw flow watch`, the GUI Runs page and Linear.

## The crw flow command table

| Command | What it does |
| --- | --- |
| `crw flow start WORKFLOW [--args JSON \| --args-file FILE] [--thread T] [--token-budget N] [--pool N] [--decision D] [--preview] [--force]` | Shows the preview (the nodes, the width, the role pairs, the budget and the pass or fail of the five preflight facts), then starts the detached run process (its own session, no inherited descriptors, the run lock) and prints the run id. `WORKFLOW` is a built-in workflow (`project-lane`, `night-batch`) or a script file. `--decision D` starts `night-batch` from the manager's decision record D. `--preview` stops after the preview; `--force` starts although a fact failed and records the override in the journal |
| `crw flow ps [--all] [--json]` | One line for each run: its state (`running`, `paused`, `dead`, `held`, `completed`, `cancelled`, `failed`), workflow, owner thread, calls and tokens. Without `--all` it lists the runs that are not over |
| `crw flow show RUN [--json]` | The tree phase, then agent: for each agent call its role, node, thread, status and reason, the commits of its node, the review verdicts, the tokens, and `recovery_hold` where a call's outcome is unknown |
| `crw flow watch RUN [--after SEQ] [--wait N] [--limit M] [--json]` | The rows of the run after a cursor, as `crw relay journal-tail` reads the relay journal: the cursor is the `seq` of the last row, `--wait` waits for a row without holding a transaction, and the next call passes the cursor back |
| `crw flow answer --decision D --run RUN --node N --question Q --revision V [--no-resume]` | Applies the manager's answer record D (`crw manage decisions`) to the wait of a run: it compares the question and its revision, the node and its attempt, the run, the plan basis and the authority of the record with the run's journal, appends one `decision_applied` row, and resumes that wait. The same answer again appends nothing |
| `crw flow pause RUN` | Asks the run process to stop sending requests; the calls in flight end held (`engine_paused`) and `crw flow resume` goes on with them |
| `crw flow cancel RUN` | Ends the run: the calls in flight are cancelled and the run finishes `cancelled` |
| `crw flow resume RUN [SCRIPT] [--detach]` | Takes the run from its journal again with the same script and arguments, once its run lock is free |
| `crw flow recover [--run RUN] CALL (--adopt THREAD\|TURN \| --absent)` | Settles a call in `recovery_hold`: the only manual way out of a call whose outcome is unknown, and the rows it appends are the evidence it relied on |
| `crw flow status-line --thread T` | The one line the Stop hook prints, when thread T owns a run that is not over; it prints nothing for any other thread |
| `crw flow run --backend fake`, `crw flow roles`, `crw flow compile`, `crw flow lint` | The fake run, the role table, the plan compiler and the script check of the engine |
| `crw flow trace export`, `crw flow shadow`, `crw flow measure` | The trace tools, which read store copies and trace files and call no agent |

## The preview and the five facts

`crw flow start` prints, before it starts anything, what the run will do and whether the way back to the orchestrator thread works. The five facts are each pass or fail with the reading behind it: (1) the assignment is routed to this thread; (2) the delivery service has ticked and has delivered; (3) the thread can be reached (its approval policy and goal state); (4) every disposition the run waits in can produce a delivery; (5) an unattended idle thread was woken into a turn in this scope. A run that cannot reach its owner would wait unseen, so a failed fact stops the start unless `--force` is given.

## Answers and waits

A wait for a decision (`crw.decide`) ends the lane's work for the run; the run goes on with that wait only when its journal holds the `decision_applied` row for the question. `crw flow answer` appends that row after it compared the record with the journal; the record (written by the manager) and the row (written for the run) are two facts. The journal holds the evidence of an applied answer, and the relay's `decision-apply` is for the oversized child's relationship only.
