"""Read an exact turn's reporting evidence without producing a report for it.

Stop is a pre-terminal observation. Only an independently settled, admitted turn
can be diagnosed as having ended without a report. This reading is neither a
receipt, a delivery acknowledgement nor acceptance of the assignment.
"""

import json
import stat
from pathlib import Path

from . import guard, intent, marker
from .admission import AnchorOrExplicit, BOUND_ADMISSION_SQL
from .store import read_only_rows
from .receipts import DAEMON

SCHEMA = "reporting-observation/1"
MAX_RECORDS = 128
MAX_FACTS = 512
MAX_BYTES = 1024 * 1024

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
        AS managed
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
    reading = read_only_rows(selection, CONTEXT, (session, turn, session, turn,
                                                relationship, dispatch))
    if not reading["readable"] or reading["detail"]:
        raise Unmeasured("store_unreadable: " + str(reading["detail"]))
    if len(reading["rows"]) != 1:
        raise Unmeasured("registration_unresolved")
    return reading


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
    if (row["child_task_id"] != session or facts["bound"].get("taskId") != session
            or row["issue_key"] != declared.get("issueKey")
            or not row["child_cwd"] or _path(row["child_cwd"]) != workspace
            or marker.assignment_id(row["dispatch_request_id"]) != assignment):
        raise Unmeasured("registry_identity_mismatch")
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


def _current(facts, disposition, selection, session, turn, now):
    receipt = None
    if disposition and disposition.get("outcome") == guard.READY:
        receipt, readable = guard.lookup_receipt(
            str(selection.db_path), relationship_id=facts["relationship"]["relationshipId"],
            session_id=session, turn_id=turn)
        if not readable:
            raise Unmeasured("receipt_unreadable")
    observation = {"stop_input": {"session_id": session, "turn_id": turn},
                   "marker": facts, "disposition": disposition, "receipt": receipt,
                   "now": now}
    label, detail = guard.observe_state(observation)
    return label, detail, receipt


def _classify(result, row):
    label = result["currentObservation"]["label"]
    settlements = json.loads(row["settlements"])
    statuses = {item["status"] for item in settlements}
    if len(statuses) > 1 or statuses - {"completed", "failed", "interrupted"}:
        raise Unmeasured("terminal_conflict")
    result["terminalObservation"] = {"source": "relay_settlement", "records": settlements,
                                     "status": next(iter(statuses), "unobserved")}
    terminal = result["terminalObservation"]["status"]
    if label == "declared_in_progress":
        return "in_progress", "declared_in_progress"
    if label.startswith("declared_"):
        return "reported", label
    events = [event for event in json.loads(row["events"])
              if event["producer"] == DAEMON and event["stage"] == "final"
              and event["outcome"] == terminal and event["status"] == terminal]
    if terminal in ("failed", "interrupted") and events:
        result["executionReports"] = events
        return "reported", "daemon_execution_report"
    if terminal == "unobserved":
        return "unmeasured", "host_terminal_unobserved"
    old = result["stopObservation"]["record"]
    omission = old["observation"] in guard.OMISSIONS or old["decisionState"] == "unresolved_handoff"
    if omission and label in guard.OMISSIONS:
        return "unreported", "terminal_without_report"
    return "unmeasured", "no_confirmed_omission"


def observe(selection, root, workspace, assignment, session, turn, now):
    """Diagnose explicit selectors; all reads are optional evidence, never writes."""
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
              "relationshipStatus": None}
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
        label, detail, receipt = _current(facts, disposition, selection, session, turn, now)
        result.update(currentObservation={"label": label, "detail": detail},
                      declaration=disposition, receipt=receipt)
        if not stops:
            state, reason = "unmeasured", "stop_unobserved"
        elif admission != "admitted":
            state, reason = "unmeasured", ("bootstrap" if admission == "bootstrap" else "admission_unrecorded")
        else:
            state, reason = _classify(result, row)
        if _context(selection, relationship, claim["dispatchRequestId"], session, turn) != snapshot:
            raise Unmeasured("registry_changed_during_read")
        result.update(reportingState=state, reason=reason)
    except Unmeasured as error:
        result.update(reportingState="unmeasured", reason=str(error))
    except (OSError, ValueError, TypeError, RuntimeError) as error:
        result.update(reportingState="unmeasured", reason="evidence_unreadable: " + str(error))
    return result
