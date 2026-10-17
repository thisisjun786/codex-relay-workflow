
## Answers and waits

A wait for a decision (`crw.decide`) ends the lane's work for the run; the run goes on with that wait only when its journal holds the `decision_applied` row for the question. `crw flow answer` appends that row after it compared the record with the journal; the record (written by the parent with `crw relay decision-answer`) and the row (written for the run) are two facts. The journal holds the evidence of an applied answer, and the relay's `decision-apply` is for the oversized child's relationship only.
