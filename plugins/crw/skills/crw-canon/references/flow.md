# The crw flow commands

`crw flow` is the command line of the flow engine: the process that runs a workflow script for one run, and the commands that start, show, follow, answer, pause, cancel, resume and recover a run. This page is the one reference of the commands; the script format is [docs/flow/script.md](../../../../../docs/flow/script.md), the journal a run writes is [docs/flow/journal.md](../../../../../docs/flow/journal.md), and the scheduler a run asks for its slots is [docs/relay/dag-scheduler.md](../../../../../docs/relay/dag-scheduler.md).

A run is a process that lives for one run and loses nothing when it dies: every fact is a row of its journal in the relay store, the run lock (`<state>/flow/<run>/run.lock`) says whether a process holds the run, and `crw flow resume` or `crw flow recover` takes a run whose lock is free from the journal again. The orchestrator thread that started a run owns it; the Stop hook prints the run's status line in that thread only, at the end of its turns. While the thread is idle, the progress is read with `crw flow watch`, the GUI Runs page and Linear.

## The crw flow command table

| Command | What it does |
| --- | --- |
| `crw flow start WORKFLOW [--args JSON \| --args-file FILE] [--thread T] [--token-budget N] [--pool N] [--decision D] [--preview] [--force]` | Shows the preview (the nodes, the width, the role pairs, the budget and the pass or fail of the five preflight facts), then starts the detached run process (its own session, no inherited descriptors, the run lock) and prints the run id. `WORKFLOW` is a built-in workflow (`project-lane`, `night-batch`) or a script file. `--decision D` starts `night-batch` from the manager's decision record D. `--preview` stops after the preview; `--force` starts although a fact failed and records the override in the journal |
| `crw flow ps [--all] [--json]` | One line for each run: its state (`running`, `paused`, `dead`, `held`, `completed`, `cancelled`, `failed`), workflow, owner thread, calls and tokens. Without `--all` it lists the runs that are not over |
| `crw flow show RUN [--json]` | The tree phase, then agent: for each agent call its role, node, thread, status and reason, the commits of its node, the review verdicts, the tokens, and `recovery_hold` where a call's outcome is unknown |
| `crw flow watch RUN [--after SEQ] [--wait N] [--limit M] [--follow] [--json]` | The same tree as `show` with the cursor of the last row it was made of; with `--after SEQ` the rows of the run after that cursor, read as `crw relay journal-tail` reads the relay journal: the cursor is the `seq` of the last row, `--wait` waits for a row without holding a transaction, and the next call passes the cursor back. `--follow` keeps reading until the run is over or has no process |
| `crw flow answer --decision D --run RUN --node N --question Q --revision V [--no-resume]` | Applies the manager's answer record D (`crw manage decisions`) to the wait of a run: it compares the question and its revision, the node and its attempt, the run, the plan basis and the authority of the record with the run's journal, appends one `decision_applied` row, and resumes that wait. The same answer again appends nothing |
| `crw flow pause RUN` | Asks the run process to stop sending requests; the calls in flight end held (`engine_paused`) and `crw flow resume` goes on with them |
| `crw flow cancel RUN` | Ends the run: the calls in flight are cancelled and the run finishes `cancelled`; the executions the run released and still owns are given up and their slots returned |
| `crw flow resume RUN [SCRIPT] [--detach]` | Takes the run from its journal again with the same script and arguments, once its run lock is free |
| `crw flow recover [--run RUN] CALL (--adopt THREAD\|TURN \| --absent)` | Settles a call in `recovery_hold`: the only manual way out of a call whose outcome is unknown, and the rows it appends are the evidence it relied on |
| `crw flow digest RUN [--out FILE]` | The run digest of a night batch (`crw-batch-digest/1`) built from the run's journal: for each selected issue whether it was integrated (with the commit, acceptance and integration the journal shows), carried (with the reason: a held call, a recovery hold, a node that did not start), cancelled or left for a decision, why the run stopped, and the token budget spent. `crw manage batch report --digest FILE` makes the morning report from it |
| `crw flow audit (--run RUN \| --acceptance ID) [--milestone M] [--json] [--no-record]` | The audit of the reviews behind the agent-path acceptances of a run, read back from the reviewer threads on the run's host (CRW-1206): each acceptance is `ok`, `mismatch` or `unobserved`, and the exit is 1 unless every one is `ok`. Once the run has finished the result is appended to its journal as a `crw.audit` row, unless `--no-record` is given |
| `crw flow status-line --thread T` | The one line the Stop hook prints, when thread T owns a run that is not over; it prints nothing for any other thread |
| `crw flow run --backend fake`, `crw flow roles`, `crw flow compile`, `crw flow lint` | The fake run, the role table, the plan compiler and the script check of the engine |
| `crw flow trace export`, `crw flow shadow`, `crw flow measure` | The trace tools, which read store copies and trace files and call no agent |

## The commands

Exit statuses: 0 did what was asked; 1 refused with a reason (a failed preflight fact, a record that does not hold, a run that is over, a run no process holds); 2 a command line, a store or a record it cannot use; 3 a process already holds the run lock. Every command takes `--state DIR` and `--socket PATH`; without them the relay's own selection is used.

### crw flow start

Shows the preview, then starts the run process: the same executable as `crw flow _run`, in a session of its own, with no descriptor of the caller's (a descriptor that cannot be kept out stops the start), no input and its output in `<state>/flow/<run>/process.log`. The spec it starts from (`<state>/flow/<run>/run.json`: the workflow, the arguments, the owner thread, the budget, the deadline, the repository) is written first and never replaced, so a resume runs the script and the arguments the run started with. The answer is the run id. A workflow that calls a `crw.*` operation (the built-in `project-lane` and `night-batch` do) is not started by a build that installs no such operations: the preview says so (`blocked`) and `--force` does not override it, because the run would end in a script error at its first call.

### crw flow ps

Lists the runs of the store. A run is `running` or `paused` while a process holds its run lock, `dead` when none does and the journal has no finished row after the last start, `held` when it finished but a call, a recovery hold or a question is left, and `completed`, `cancelled` or `failed` when it is over.

### crw flow show

The tree of one run: its phases in the order the script opened them, the agent calls made in each (role, node, status and reason, thread, tokens), and under them what each node has: the commits, the review verdicts with the reviewer's call, the acceptance and the integration, the tokens. A call whose outcome is unknown, and the node it belongs to, are marked `recovery_hold`.

### crw flow watch

Without `--after`, the tree of `show` and the cursor it was made at. With `--after`, one read of the run's journal after that cursor: at most `--limit` rows, `--wait` seconds for a row when there is none, and the cursor line to pass back. `--follow` prints the tree, then keeps reading from its cursor until the run is over or has no process, one line for each row, and ends with the tree again. The rows carry no call results; the journal itself holds them.

### crw flow answer

See [How an answer names its question](#how-an-answer-names-its-question). The answer is applied to the journal of a run no process holds: a run a process holds is refused (`crw flow pause` first), the row is appended under the run lock, and the run is resumed in the background unless `--no-resume` is given. The refusals are `not_answered`, `bad_binding`, `other_run`, `other_node`, `unknown_question`, `stale_revision`, `stale_attempt`, `stale_plan_basis`, `out_of_scope_authority` and `already_applied`.

### crw flow pause

Signals the process that holds the run lock (SIGUSR1). The process stops sending: the turn in flight is interrupted and the calls that would send end held `engine_paused`; the run then finishes `held`, and `crw flow resume` goes on with those calls. A run no process holds is refused, since nothing runs.

### crw flow cancel

Signals the process (SIGTERM): the calls in flight are cancelled and the run finishes `cancelled`. A process whose lock names no pid is never signalled (a pid of 0 would be the caller's own process group); the command says so and the operator ends the process by hand. A run no process holds is cancelled by a finished row appended under the run lock, after the executions its journal shows it released and did not end have been closed with `flow_cancel` (the slot returns in the same transaction); when that fails the run stays as it was and the command can be repeated. An execution is given up only while it is the run's: its run id is the cancelled run's and it is the execution the run released (or follows it in the node's chain). A node that another run released again after this run's cancel keeps its execution and its slot, so the second cancel of a cancelled run never takes them. A run that is already cancelled is settled the same way once more and then refused as over.

### crw flow resume

Takes the run from its journal once its run lock is free: with the run's own spec (`crw flow resume RUN`, in the foreground, or `--detach`), or with a script and arguments given (`crw flow resume RUN SCRIPT`, the engine's command, which refuses a script or arguments that are not the run's). Both go on under the budget and the deadline of the run's spec (a night batch's are its decision's): `crw flow resume RUN SCRIPT` takes the spec's budget when no `--token-budget` is given, refuses a `--token-budget` of 0, below 0 or above the spec's (a lower one narrows it), and pauses the run at the spec's deadline. Every call whose last row is an intent is looked up on the host before the script runs again; a call in recovery hold stays until `crw flow recover` settles it.

### crw flow recover

The engine's recovery of a call in `recovery_hold`, under the run lock of the run that holds the call. `--adopt THREAD|TURN` takes the thread or turn the operator names after the proofs of the call's cwd, source and history; `--absent` reads the host again and accepts the call only when the same proofs hold as for an automatic outcome. What it relied on is appended to the journal as `recovery_cleared` with its evidence.

### crw flow status-line

The line the Stop hook prints, `crw flow <run> <state> · <phase> · <n> calls · ...`, for the runs of thread T that are not over, and nothing for any other thread.

### crw flow digest

The digest of a night batch; see [The night batch](#the-night-batch).

## The preview and the five facts

`crw flow start` prints, before it starts anything, what the run will do and whether the way back to the orchestrator thread works. The five facts are each pass or fail with the reading behind it: (1) the assignment is routed to this thread; (2) the delivery service has ticked and has delivered; (3) the thread can be reached (its approval policy and goal state); (4) every disposition the run waits in can produce a delivery; (5) an unattended idle thread was woken into a turn in this scope. A run that cannot reach its owner would wait unseen, so a failed fact stops the start unless `--force` is given.

## Answers and waits

A wait for a decision (`crw.decide`) ends the lane's work for the run; the run goes on with that wait only when its journal holds the `decision_applied` row for the question. `crw flow answer` appends that row after it compared the record with the journal; the record (written by the manager) and the row (written for the run) are two facts. The journal holds the evidence of an applied answer, and the relay's `decision-apply` is for the oversized child's relationship only.

## The night batch

A night batch is a decision and a run. The manager selects the issues (`crw manage batch plan`, 1040a) and records the decision with `crw manage batch decide --plan FILE --flow-plan PLAN --milestone M --token-budget N --deadline T`: a user-decision record (kind `policy`, options `run` and `skip`) whose context is the batch document (`crw-night-batch/1`: the label, the issues, the plan and milestone whose nodes they are, the token budget, the deadline), raised and answered `run` through the relay's decision commands. `crw flow start night-batch --decision D` reads that record, refuses one that is not answered `run`, and starts the run for those issues with the decision's budget and deadline; `--token-budget` and `--deadline` may only lower the budget and bring the deadline forward, a larger budget, a later deadline and a negative budget are refused. The run is the project lane over the milestone, after a check that the milestone's nodes are exactly the selected issues; releases happen only inside it, so there is no freeze and no grant table. The run stops at its token budget (the calls that would send end held `budget_exhausted`) and pauses at its deadline; what did not land is carried, with the reason. A milestone whose nodes are not the selection releases nothing: the run asks the manager (`night_batch_selection`), stays `held`, and its digest lists every issue as needing a decision with the plan's difference. `crw flow digest RUN` and `crw manage batch report --digest FILE` make the morning report. A run whose process was killed is taken again by `crw flow resume RUN`, builds nothing it already landed, and goes on with the rest.

## How an answer names its question

The record binds the question it answers in the members the relay's record has: one `plan_node` entry of `blocking` (`<run>/<node>`), and a `source` of kind `flow_question` (an escalation answer) or `flow_jun_decision` (a decision only Jun takes, recorded by the manager) whose ref is `<question id>@<revision>#<attempt>:<plan basis>`. `crw flow answer` compares that with what the caller claims and with the run's journal: the run, the node, the question and its revision (the question must be the latest of its node and ask), the attempt of the node it was asked in, the plan basis (the digest of the plan id and revision the run is pinned to), and the authority. An escalation is answered by the orchestrator that holds the run (class `parent`, ref the owner thread), the manager (`delegated-management`) or Jun (`user`); a decision only Jun takes needs the `user` class and a way the user's answer arrives (`dots`, `direct-ask`, `file-import`, `management-message`), never the parent's report read back. The first difference is the refusal, and nothing is appended.
