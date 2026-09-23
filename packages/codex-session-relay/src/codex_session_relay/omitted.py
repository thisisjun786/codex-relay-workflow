"""Read an exact turn's reporting evidence without producing a report for it.

Stop is a pre-terminal observation. Only an independently settled, admitted turn
can be diagnosed as having ended without a report. This reading is neither a
receipt, a delivery acknowledgement nor acceptance of the assignment.

Two readers, one diagnosis. observe() reads a turn through the marker and this store, the way a
caller who holds the marker selectors can; derive() reads the same turn from this store alone,
which is what the relay daemon can do - it reads no marker file. Both gather the same facts and
hand them to classify(), the one predicate that decides what the turn's evidence says and whether
a report is still owed because of it. Neither carries a classifier of its own.
"""

import json
import stat
from pathlib import Path

from . import guard, intent, marker
from .admission import AnchorOrExplicit, BOUND_ADMISSION_SQL
from .store import read_only_rows
from .receipts import CHILD as CHILD_PRODUCER, DAEMON

SCHEMA = "reporting-observation/1"
MAX_RECORDS = 128
MAX_FACTS = 512
MAX_BYTES = 1024 * 1024

# The one reading derive() produces, and the only thing that tells a store reading from a marker
# reading once either is frozen on a staged message.
STORE_SOURCE = "relay_store"
# The capability a child's relay records when it claims (declarations.CAPABILITY). Repeated here
# for the same reason supervision repeats this module's schema: importing the writer for one
# string would pull the Store into every reader of the diagnosis.
CAPABILITY = "declarations/1"
TERMINAL_STATUSES = ("completed", "failed", "interrupted")

# What classify() answers about whether a report is still owed, beside the diagnosis itself. The
# diagnosis of a turn does not change because something happened after it; what is owed does.
OWED = "terminal_without_report"
NOT_AN_OMISSION = "not_an_omission"
LATER_TURN_ADMITTED = "later_turn_admitted"
TURN_RECEIPTED = "turn_receipted"
WITHIN_GRACE = "within_report_grace"
GRACE_UNMEASURED = "report_grace_unmeasured"
# derive()'s answer for a session whose relay never recorded that it writes declarations here:
# the cut-over. Named, because the channel treats it apart from every other unmeasured reading.
DECLARATIONS_NOT_RECORDED = "declarations_not_recorded"

# One SELECT gives the registry, admission and terminal facts the same SQLite
# snapshot. Filesystem reads happen afterwards, with this snapshot rechecked.
CONTEXT = """
SELECT r.relationship_id, r.issue_key, r.status, r.parent_task_id, r.child_task_id,
       r.child_cwd, r.execution_generation, r.superseded_by,
       g.execution_generation AS opened_generation, g.dispatch_request_id,
       g.dispatch_turn_id, g.anchor_state,
       (SELECT json_group_array(turn_id) FROM generation_turns t
        WHERE t.relationship_id=r.relationship_id
          AND t.execution_generation=g.execution_generation
          AND """ + BOUND_ADMISSION_SQL + """) AS admitted,
       (SELECT json_group_array(json_object('status',s.terminal_status,'at',s.settled_at))
        FROM assignment_settlements s WHERE s.relationship_id=r.relationship_id
          AND s.thread_id=? AND s.turn_id=?) AS settlements,
       (SELECT json_group_array(json_object('eventId',e.event_id,'outcome',e.outcome,
                 'stage',e.stage,'producer',e.producer,'status',e.turn_status))
        FROM events e WHERE e.relationship_id=r.relationship_id
          AND e.execution_generation=g.execution_generation
          AND e.turn_thread_id=? AND e.turn_id=?) AS events,
       (SELECT json_group_array(json_object('requestId',m.request_id,'state',m.state,
                 'child',m.child_task_id,'standby',m.standby_turn_id,
                 'relationship',m.relationship_id,'generation',m.execution_generation,
                 'workspace',m.workspace,'markerRoot',m.marker_root,'issue',m.issue_key))
        FROM managed_start_requests m WHERE m.dispatch_request_id=g.dispatch_request_id)
        AS managed,
       (SELECT COUNT(*) FROM generation_turns t
        WHERE t.relationship_id=r.relationship_id
          AND t.execution_generation=g.execution_generation
          AND """ + BOUND_ADMISSION_SQL + """
          AND t.turn_id<>?
          AND NOT EXISTS (SELECT 1 FROM generation_turns a
                          WHERE a.relationship_id=r.relationship_id
                            AND a.execution_generation=g.execution_generation
                            AND a.turn_id=?
                            AND (julianday(a.admitted_at)>julianday(t.admitted_at)
                                 OR (julianday(a.admitted_at)=julianday(t.admitted_at)
                                     AND a.rowid>=t.rowid)))) AS later_admitted
FROM relationships r JOIN generations g ON g.relationship_id=r.relationship_id
WHERE r.relationship_id=? AND g.dispatch_request_id=?
"""


class Unmeasured(Exception):
    """A named evidence read failed; absence must not be inferred from it."""


def _path(value):
    return Path(value).expanduser().resolve()


def _confined_facts(directory, root, session, turn):
    """Check only identity facts and this turn's evidence before existing readers.

    Other turns' Stop/disposition history is not consumed and cannot exhaust this
    reading's budget. Shared claims/attempts/conflicts still establish identity.
    Same-user evidence is not authentication against concurrent hostile writes.
    """
    def checked(path):
        marker.confined(path, root)
        for ancestor in (path, *path.parents):
            if ancestor == root:
                break
            if ancestor.is_symlink():
                raise Unmeasured("marker_symlink")
        try:
            metadata = path.stat()
        except FileNotFoundError:
            return None
        if not (stat.S_ISDIR(metadata.st_mode) or stat.S_ISREG(metadata.st_mode)):
            raise Unmeasured("marker_not_regular")
        if stat.S_ISREG(metadata.st_mode) and metadata.st_size > MAX_BYTES:
            raise Unmeasured("marker_record_limit")
        return metadata

    checked(directory)
    for name in marker.SINGLE_FACTS.values():
        checked(directory / name)
    checked(directory / "dispositions" / session / (turn + ".json"))
    count = 0

    def entries(folder, pattern=None, only=None):
        nonlocal count
        checked(folder)
        paths, readable = marker.listing(folder, pattern, only=only)
        if not readable:
            raise Unmeasured("marker_listing_unreadable")
        for path in paths:
            if pattern and path.name.startswith("."):
                continue
            if folder == directory / "hook" / session / turn and path.name == guard.HOLD_FILE:
                continue
            count += 1
            if count > MAX_FACTS:
                raise Unmeasured("marker_history_limit")
            yield path

    for name in marker.NUMBERED_FACTS:
        for path in entries(directory / name, "*.json"):
            checked(path)
    for claim_dir in entries(directory / "claims", only=marker.DIRECTORIES):
        checked(claim_dir / marker.CLAIM_FILE)
    for path in entries(directory / "hook" / session / turn, "*.json"):
        if path.name != guard.HOLD_FILE:
            checked(path)


def _stops(directory, session, turn):
    folder = directory / "hook" / session / turn
    paths, readable = marker.listing(folder, "*.json")
    if not readable:
        raise Unmeasured("stop_unreadable")
    records = []
    for path in paths:
        if path.name == guard.HOLD_FILE or path.name.startswith("."):
            continue
        if not path.stem.isdecimal() or len(records) >= MAX_RECORDS:
            raise Unmeasured("stop_history_invalid")
        marker.confined(path, directory)
        with path.open("rb") as handle:
            raw = handle.read(MAX_BYTES + 1)
        if len(raw) > MAX_BYTES:
            raise Unmeasured("stop_record_limit")
        value = json.loads(raw)
        if (not isinstance(value, dict) or value.get("sessionId") != session
                or value.get("turnId") != turn
                or not isinstance(value.get("observation"), str)
                or not isinstance(value.get("decisionState"), str)
                or intent.moment(value.get("at")) is None):
            raise Unmeasured("stop_identity_or_shape")
        records.append({"sequence": int(path.stem), "path": str(path), "record": value})
    records.sort(key=lambda entry: entry["sequence"])
    if len({entry["sequence"] for entry in records}) != len(records):
        raise Unmeasured("stop_sequence_ambiguous")
    return records


def _context(selection, relationship, dispatch, session, turn):
    reading = read_only_rows(selection, CONTEXT, _context_params(relationship, dispatch,
                                                                 session, turn))
    if not reading["readable"] or reading["detail"]:
        raise Unmeasured("store_unreadable: " + str(reading["detail"]))
    if len(reading["rows"]) != 1:
        raise Unmeasured("registration_unresolved")
    return reading


def _context_params(relationship, dispatch, session, turn):
    """CONTEXT's parameters, in the order its placeholders appear."""
    return (session, turn, session, turn, turn, turn, relationship, dispatch)


class _AdmissionReader:
    """Expose already snapshotted admission rows to the existing policy owner."""

    def __init__(self, row):
        self.row = row
        self.admitted = json.loads(row["admitted"])

    def one(self, sql, params):
        rid, generation, turn = params
        if (rid != self.row["relationship_id"]
                or generation != self.row["opened_generation"]):
            raise Unmeasured("admission_identity_changed")
        return {"present": 1} if turn in self.admitted else None


def _registry_evidence(row, facts, root, workspace, assignment, session, turn):
    declared = facts["intent"]
    problem = _identity_problem(row, session=session, bound_task=facts["bound"].get("taskId"),
                                issue_key=declared.get("issueKey"), workspace=workspace,
                                assignment=assignment)
    if problem:
        raise Unmeasured(problem)
    return _admission(row, session, root, workspace, turn)


def _identity_problem(row, *, session, bound_task, issue_key, workspace, assignment):
    """Whether the relay's registration is the assignment this reading is about, or why not.

    Shared by both readers, so neither classifies a turn the other would refuse to. The
    registered child must be the session, the task the coordinator bound must be that session
    too, and the registration's issue, working directory and dispatch must be the ones the
    assignment was declared with. The marker reader takes the binding and the declared issue
    from the marker; the store reader takes the binding from the registration itself - the
    coordinator's statement of who the child is, in this store - and the issue and workspace
    from the claim record the child's relay wrote beside the marker (declarations.py).
    """
    if (row["child_task_id"] != session or bound_task != session
            or row["issue_key"] != issue_key
            or not row["child_cwd"] or _path(row["child_cwd"]) != workspace
            or marker.assignment_id(row["dispatch_request_id"]) != assignment):
        return "registry_identity_mismatch"
    return None


def _admission(row, session, root, workspace, turn):
    """Whether this turn belongs to the execution, read off the snapshotted CONTEXT row.

    Shared by both readers. The marker reader checks the marker's identity facts first; the
    store reader has no marker, and checks the session and the paths its claim recorded instead.
    """
    if row["execution_generation"] != row["opened_generation"] or row["superseded_by"]:
        raise Unmeasured("stale_generation")
    requests = json.loads(row["managed"])
    if len(requests) > 1:
        raise Unmeasured("managed_request_ambiguous")
    bootstrap = False
    if requests:
        request = requests[0]
        if (request["child"] != session or request["relationship"] != row["relationship_id"]
                or request["generation"] != row["opened_generation"]
                or request["issue"] != row["issue_key"]
                or _path(request["workspace"]) != workspace
                or _path(request["markerRoot"]) != root):
            raise Unmeasured("managed_request_identity_mismatch")
        bootstrap = request["standby"] == turn
    generation = {"dispatchTurnId": row["dispatch_turn_id"],
                  "executionGeneration": row["opened_generation"]}
    relation = {"relationshipId": row["relationship_id"]}
    admitted = AnchorOrExplicit().admit(_AdmissionReader(row), relation, generation, turn)
    return ("bootstrap" if bootstrap else "admitted" if admitted.admitted else "unadmitted"), requests


def _current(facts, disposition, selection, assignment, session, turn, now):
    receipt = None
    if disposition and disposition.get("outcome") == guard.READY:
        receipt, readable = guard.lookup_receipt(
            str(selection.db_path), relationship_id=facts["relationship"]["relationshipId"],
            session_id=session, turn_id=turn,
            execution_generation=facts["relationship"].get("executionGeneration"),
            dispatch_request_id=intent.claimed_dispatch(facts, session, assignment))
        if not readable:
            raise Unmeasured("receipt_unreadable")
    observation = {"stop_input": {"session_id": session, "turn_id": turn},
                   "marker": facts, "disposition": disposition, "receipt": receipt,
                   "now": now}
    label, detail = guard.observe_state(observation)
    return label, detail, receipt


def _terminal(settlements):
    """The one terminal status the relay settled this turn with, or None when they conflict."""
    statuses = {item["status"] for item in settlements}
    if len(statuses) > 1 or statuses - set(TERMINAL_STATUSES):
        return None
    return next(iter(statuses), "unobserved")


def _execution_reports(row, terminal):
    """The daemon's own final events stating the ending the relay settled."""
    return [event for event in json.loads(row["events"])
            if event["producer"] == DAEMON and event["stage"] == "final"
            and event["outcome"] == terminal and event["status"] == terminal]


def facts_of(row, *, witness, admission, label, now, grace):
    """The facts classify() decides from, gathered off one CONTEXT row.

    Everything that is not specific to where the declaration was read comes from this row, so
    both readers hand classify() facts assembled the same way. witness is the reader's own: the
    marker reader's is the last Stop record, the store reader's is its declaration record.
    """
    settlements = json.loads(row["settlements"])
    terminal = _terminal(settlements)
    events = json.loads(row["events"])
    return {
        "witness": witness,
        "admission": admission,
        "settlements": settlements,
        "label": label,
        "executionReport": bool(terminal and _execution_reports(row, terminal)),
        # A final receipt from the child for this very turn. The channel's I-247 recheck has
        # always treated one as answering an omission; the predicate now says so for everyone.
        "receipted": any(event["producer"] == CHILD_PRODUCER and event["stage"] == "final"
                         for event in events),
        "laterAdmitted": bool(row["later_admitted"]),
        "now": now,
        "grace": float(grace or 0),
    }


def _answer(state, reason, owed_reason=None):
    owed = state == "unreported" and owed_reason is None
    return {"reportingState": state, "reason": reason, "owed": owed,
            "owedReason": OWED if owed else (owed_reason or NOT_AN_OMISSION)}


def classify(facts) -> dict:
    """What one turn's evidence says, and whether a report is still owed because of it. Pure.

    The ONE predicate. observe() and derive() both call it, with facts assembled by facts_of();
    neither decides anything about the turn itself.

    facts:
      witness         None when nothing witnessed the turn ending; otherwise whether it ended as
                      an omission (the Stop record for the marker reader, the recorded
                      declaration for the store reader)
      admission       admitted, bootstrap or unadmitted
      settlements     the terminal statuses the relay settled this turn with
      label           the turn's declaration now, in the guard's vocabulary
      executionReport the daemon's own final event states the settled ending
      receipted       the child's final receipt for this turn exists
      laterAdmitted   a later turn was admitted to the same generation
      now, grace      when this is read, and how long after settlement an omission waits

    reportingState and reason are CRW-180's diagnosis, unchanged. owed says whether the
    diagnosis still leaves a report owed, and owedReason why not: a receipt for the turn goes up
    as its own fact, a later admitted turn means the work went on, and an omission younger than
    the grace has not yet had the chance to be answered. A diagnosis is never rewritten by what
    happened after the turn; only what is owed is.
    """
    if facts["witness"] is None:
        return _answer("unmeasured", "stop_unobserved")
    if facts["admission"] != "admitted":
        return _answer("unmeasured", "bootstrap" if facts["admission"] == "bootstrap"
                       else "admission_unrecorded")
    terminal = _terminal(facts["settlements"])
    if terminal is None:
        return _answer("unmeasured", "terminal_conflict")
    label = facts["label"]
    if label == "declared_in_progress":
        return _answer("in_progress", "declared_in_progress")
    if label.startswith("declared_"):
        return _answer("reported", label)
    if terminal in ("failed", "interrupted") and facts["executionReport"]:
        return _answer("reported", "daemon_execution_report")
    if terminal == "unobserved":
        return _answer("unmeasured", "host_terminal_unobserved")
    if not (facts["witness"] and label in guard.OMISSIONS):
        return _answer("unmeasured", "no_confirmed_omission")
    if facts["receipted"]:
        return _answer("unreported", "terminal_without_report", TURN_RECEIPTED)
    if facts["laterAdmitted"]:
        return _answer("unreported", "terminal_without_report", LATER_TURN_ADMITTED)
    if facts["grace"] > 0:
        ended = [intent.moment(item.get("at")) for item in facts["settlements"]]
        now = intent.moment(facts["now"])
        if now is None or not ended or any(one is None for one in ended):
            return _answer("unreported", "terminal_without_report", GRACE_UNMEASURED)
        if (now - max(ended)).total_seconds() < facts["grace"]:
            return _answer("unreported", "terminal_without_report", WITHIN_GRACE)
    return _answer("unreported", "terminal_without_report")


def _stop_witness(stops):
    """The marker reader's witness: whether the last Stop record saw an omission, or None."""
    if not stops:
        return None
    old = stops[-1]["record"]
    return old["observation"] in guard.OMISSIONS or old["decisionState"] == "unresolved_handoff"


def _record_terminal(result, row, verdict):
    """What the reading shows about the ending, where classification got far enough to read it."""
    if verdict["reason"] in ("stop_unobserved", "bootstrap", "admission_unrecorded",
                             "terminal_conflict"):
        return
    settlements = json.loads(row["settlements"])
    terminal = _terminal(settlements)
    result["terminalObservation"] = {"source": "relay_settlement", "records": settlements,
                                     "status": terminal}
    if verdict["reason"] == "daemon_execution_report":
        result["executionReports"] = _execution_reports(row, terminal)


def observe(selection, root, workspace, assignment, session, turn, now, grace=0):
    """Diagnose explicit selectors; all reads are optional evidence, never writes.

    grace is classify()'s: how long after settlement an omission waits before it is owed. A
    caller asking about one turn gets the diagnosis at once unless it asks for the grace.
    """
    if not marker.valid_assignment(assignment):
        raise ValueError("assignment must be a dispatch hash")
    if not marker.valid_segment(session) or not marker.valid_segment(turn):
        raise ValueError("session and turn must be valid path segments")
    if not root or not workspace:
        raise ValueError("marker root and workspace are required")
    result = {"schema": SCHEMA, "reportingState": "unmeasured", "reason": None,
              "observedAt": now, "selectors": {"state": str(selection.path),
              "markerRoot": str(root), "workspace": str(workspace), "assignment": assignment,
              "session": session, "turn": turn}, "stopObservation": None,
              "terminalObservation": {"source": "relay_settlement", "status": "unobserved"},
              "currentObservation": None, "turnAdmission": "unmeasured",
              "relationshipStatus": None, "owed": False, "owedReason": NOT_AN_OMISSION}
    try:
        root, workspace = _path(root), _path(workspace)
        directory = marker.assignment_dir(root, workspace, assignment)
        _confined_facts(directory, root, session, turn)
        facts, unreadable = marker.read_assignment(directory)
        if unreadable:
            raise Unmeasured("marker_unreadable")
        if "intent" not in facts:
            result.update(reportingState="unmanaged", reason="marker_absent")
            return result
        if intent.malformed(facts):
            raise Unmeasured("marker_malformed")
        if not intent.correlated(facts, session):
            raise Unmeasured("dispatch_uncorrelated")
        if not facts.get("bound") or facts["bound"].get("sessionId") != session:
            raise Unmeasured("session_unbound_or_foreign")
        declared = facts["intent"]
        if (declared.get("dispatchRequestIdHash") != assignment
                or not declared.get("workspace") or _path(declared["workspace"]) != workspace
                or not declared.get("dbPath") or _path(declared["dbPath"]) != _path(selection.db_path)):
            raise Unmeasured("marker_selector_mismatch")
        stops = _stops(directory, session, turn)
        if stops:
            result["stopObservation"] = stops[-1]
            result["stopRecordCount"] = len(stops)
        relationship = (facts.get("relationship") or {}).get("relationshipId")
        if not relationship:
            raise Unmeasured("registration_unresolved")
        claim = next(c for c in facts["claims"] if intent.claimant(c) == session
                     and marker.assignment_id(c.get("dispatchRequestId")) == assignment)
        snapshot = _context(selection, relationship, claim["dispatchRequestId"], session, turn)
        row = snapshot["rows"][0]
        result.update(relationshipId=relationship, relationshipStatus=row["status"],
                      executionGeneration=row["opened_generation"], parentTaskId=row["parent_task_id"])
        admission, requests = _registry_evidence(row, facts, root, workspace, assignment, session, turn)
        result.update(turnAdmission=admission, managedRequests=requests)
        disposition, readable = marker.read_disposition(directory, session, turn)
        if not readable or intent.malformed_disposition(disposition):
            raise Unmeasured("disposition_unreadable_or_malformed")
        label, detail, receipt = _current(
            facts, disposition, selection, assignment, session, turn, now)
        result.update(currentObservation={"label": label, "detail": detail},
                      declaration=disposition, receipt=receipt)
        verdict = classify(facts_of(row, witness=_stop_witness(stops), admission=admission,
                                    label=label, now=now, grace=grace))
        _record_terminal(result, row, verdict)
        if _context(selection, relationship, claim["dispatchRequestId"], session, turn) != snapshot:
            raise Unmeasured("registry_changed_during_read")
        result.update(verdict)
    except Unmeasured as error:
        result.update(_answer("unmeasured", str(error)))
    except (OSError, ValueError, TypeError, RuntimeError) as error:
        result.update(_answer("unmeasured", "evidence_unreadable: " + str(error)))
    return result


# ------------------------------------------------------------------ from this store alone

CURRENT = """
SELECT r.relationship_id, r.status, r.parent_task_id, r.child_task_id, r.issue_key,
       r.execution_generation, r.superseded_by, g.dispatch_request_id, g.dispatch_turn_id
FROM relationships r JOIN generations g ON g.relationship_id=r.relationship_id
 AND g.execution_generation=r.execution_generation
WHERE r.relationship_id=?
"""

# The newest turn admitted to a generation by an explicit bound record, in admission order.
# Every other admitted turn of the generation has a later one, so it is the only turn whose
# omission can still be owed; the anchor is that turn when nothing was admitted after it.
#
# Admission order is the bound admission's time, rowid breaking ties. The rowid alone is
# insertion order, and a legacy row repaired by a fresh admission keeps the rowid it was first
# inserted with (admission._record_bound upserts), so ordering by it put a turn admitted last
# behind one admitted before it and reported the earlier turn's omission after work went on.
# later_admitted in CONTEXT asks the same order.
LATEST_ADMITTED = """
SELECT t.turn_id FROM generation_turns t JOIN generations g
  ON g.relationship_id=t.relationship_id AND g.execution_generation=t.execution_generation
WHERE t.relationship_id=? AND t.execution_generation=? AND """ + BOUND_ADMISSION_SQL + """
ORDER BY julianday(t.admitted_at) DESC, t.rowid DESC LIMIT 1
"""


def derive(store, relationship_id, *, state_directory, now, grace, turn=None):
    """The reading observe() would give, taken from this store alone. Reads; never writes.

    The relay daemon reads no marker file, so it cannot call observe(). What it can read is
    what the child's own relay recorded here beside the marker (declarations.py): that this
    session records its declarations in this store, and what each turn declared. With those,
    the facts classify() needs are all in this store, and they go through the same predicate.

    The cut-over is the claim record. A turn whose session never recorded one - every child
    that claimed before its relay wrote here, or through a relay that does not - is a legacy
    admission: its declarations may exist only in the marker, so this store's silence about
    them proves nothing and nothing is derived. It stays owed and visible exactly as before,
    through a reading somebody passes in.

    turn defaults to the relationship's newest admitted turn, which is the only one whose
    omission can still be owed. The store's witness of the ending is its declaration record:
    there is no pre-terminal record here like the marker's Stop record, and the relay's own
    settlement is what says the turn ended.
    """
    result = {"schema": SCHEMA, "source": STORE_SOURCE, "reportingState": "unmeasured",
              "reason": None, "observedAt": now, "selectors": None,
              "relationshipId": relationship_id, "relationshipStatus": None,
              "terminalObservation": {"source": "relay_settlement", "status": "unobserved"},
              "currentObservation": None, "turnAdmission": "unmeasured",
              "declaration": None, "receipt": None,
              "owed": False, "owedReason": NOT_AN_OMISSION}
    try:
        current = store.one(CURRENT, (relationship_id,))
        if current is None:
            raise Unmeasured("registration_unresolved")
        result.update(relationshipStatus=current["status"],
                      executionGeneration=current["execution_generation"],
                      parentTaskId=current["parent_task_id"])
        session = current["child_task_id"]
        dispatch = current["dispatch_request_id"]
        assignment = marker.assignment_id(dispatch)
        claimed = store.one(
            "SELECT * FROM reporting_sessions WHERE assignment_id=? AND session_id=?",
            (assignment, session))
        if (claimed is None or claimed["dispatch_request_id"] != dispatch
                or claimed["capability"] != CAPABILITY):
            raise Unmeasured(DECLARATIONS_NOT_RECORDED)
        if turn is None:
            latest = store.one(LATEST_ADMITTED, (relationship_id,
                                                 current["execution_generation"]))
            turn = latest["turn_id"] if latest is not None else current["dispatch_turn_id"]
        if not marker.valid_segment(turn or ""):
            raise Unmeasured("admission_unrecorded")
        result["selectors"] = {"state": str(state_directory),
                               "markerRoot": claimed["marker_root"],
                               "workspace": claimed["workspace"], "assignment": assignment,
                               "session": session, "turn": turn}
        rows = store.all(CONTEXT, _context_params(relationship_id, dispatch, session, turn))
        if len(rows) != 1:
            raise Unmeasured("registration_unresolved")
        row = rows[0]
        problem = _identity_problem(row, session=session, bound_task=current["child_task_id"],
                                    issue_key=claimed["issue_key"],
                                    workspace=_path(claimed["workspace"]), assignment=assignment)
        if problem:
            raise Unmeasured(problem)
        admission, requests = _admission(row, session, _path(claimed["marker_root"]),
                                         _path(claimed["workspace"]), turn)
        result.update(turnAdmission=admission, managedRequests=requests)
        declared = store.one(
            "SELECT outcome, declared_at, recorded_at FROM turn_declarations"
            " WHERE assignment_id=? AND session_id=? AND turn_id=?",
            (assignment, session, turn))
        disposition = None if declared is None else {
            "sessionId": session, "turnId": turn, "outcome": declared["outcome"],
            "at": declared["declared_at"], "recordedAt": declared["recorded_at"]}
        receipt = None
        if disposition and disposition["outcome"] == guard.READY:
            receipt, readable = guard.lookup_receipt(
                str(store.path), relationship_id=relationship_id, session_id=session,
                turn_id=turn, execution_generation=current["execution_generation"],
                dispatch_request_id=dispatch)
            if not readable:
                raise Unmeasured("receipt_unreadable")
        label = guard.classify_declaration({
            "stop_input": {"session_id": session, "turn_id": turn},
            "disposition": disposition, "receipt": receipt,
            "marker": {"relationship": {"relationshipId": relationship_id}}})
        result.update(currentObservation={"label": label}, declaration=disposition,
                      receipt=receipt)
        verdict = classify(facts_of(row, witness=label in guard.OMISSIONS,
                                    admission=admission, label=label, now=now, grace=grace))
        _record_terminal(result, row, verdict)
        result.update(verdict)
    except Unmeasured as error:
        result.update(_answer("unmeasured", str(error)))
    except (OSError, ValueError, TypeError, RuntimeError) as error:
        result.update(_answer("unmeasured", "evidence_unreadable: " + str(error)))
    return result


def owed_in_project(store, project_key, *, state_directory, now, grace) -> list:
    """Every store reading in one project that leaves a report owed, and nothing else.

    One reading per relationship, of its newest admitted turn. A reading that owes nothing -
    reported, still running, legacy, inside its grace - is not returned: this is what the
    automatic pass stages from, and a list of everything it did NOT owe would be a gap per
    relationship per tick.
    """
    owed = []
    for row in store.all(
            "SELECT r.relationship_id FROM relationships r"
            "  JOIN relationship_scope s ON s.relationship_id = r.relationship_id"
            " WHERE s.project_key = ? ORDER BY r.created_at", (project_key,)):
        reading = derive(store, row["relationship_id"], state_directory=state_directory,
                         now=now, grace=grace)
        if reading["reportingState"] == "unreported" and reading["owed"]:
            owed.append(reading)
    return owed
