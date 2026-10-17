# The crw flow commands

Here the parent (repository session) and the child (project session) are layers of [the level table](../hierarchy.md#the-levels).

`crw flow` is the command line of the flow engine: the process that runs a workflow script for one run, and the commands that start, show, follow, answer, pause, cancel, resume and recover a run. This page is the one reference of the commands; the script format is `docs/flow/script.md` in the CRW repository, the journal a run writes is `docs/flow/journal.md` in the CRW repository, and the scheduler a run asks for its slots is `docs/relay/dag-scheduler.md` in the CRW repository. The agent calls a run makes, and why, are the [spawn policy](../spawn-policy.md).

A run is a process that lives for one run and loses nothing when it dies: every fact is a row of its journal in the relay store, the run lock (`<state>/flow/<run>/run.lock`) says whether a process holds the run, and `crw flow resume` or `crw flow recover` takes a run whose lock is free from the journal again. The child that started a run, its owner thread, owns it; the Stop hook prints the run's status line in that thread only, at the end of its turns. While the thread is idle, the progress is read with `crw flow watch`, the GUI Runs page and the comments of the tracking issues.
