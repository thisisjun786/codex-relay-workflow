"""What a parent owes the level above, staged before it is sent, and read back.

supervision.py decides WHAT is owed upward and delivery.py carries the parent-child relation.
Between them there was nothing. A parent that had decided it owed a report had nowhere to put
it, and no row in this store could ever say a supervisor received one; envelope.REACH_SOURCES
said so in data and supervision.py said so in prose. This is that missing half, and it is
deliberately the smaller half.

One direction. A report travels parent to supervisor. An instruction travelling the other way
is a scope_directives row that linkage already owns, and nothing here sends one.

What is staged is a relay-packet/1 frozen before any transport call, so a crash between
deciding and sending loses the send and not the decision. The message id is the envelope's,
derived from the direction, the relation, the purpose and the subject, so one fact staged twice
converges on one row rather than waking a supervisor twice.

What a verified readback establishes, written here because the word "received" invites more
than it holds: this attempt's request id is in the recipient's own transcript, the named turn is
real on its thread, and that turn did not certainly begin before the send. It does not establish
that the turn answered anything or who computed the proof. The proof is built from two
identifiers this store holds, and the turn a send opens is one whose id the sender already
knows - so for that turn, which is the ordinary case, a verified readback needs no act of the
supervisor's at all and shows arrival rather than reading. Which turn was named is recorded
rather than averaged into one word.

Nothing here wakes anybody on a timer. There is no daemon pass behind these methods: a report
goes out inside the parent's own turn, and what discharges the obligation is still the Linear
record the supervisor reads for itself, confirmed, exactly as supervision.discharge_of decides.
"""

import json
import math
import os
import shlex
from datetime import datetime

from . import envelope, packets, supervision
from .ack import TURN_START_PRECISION_SECONDS
from .delivery import authorized_settings, reserve_send, send_refusal
from .errors import DeliveryRefused, RefusalReason
from .identity import supervisor_read_proof, supervisor_request_id
from .lifecycle import observe, record as record_lifecycle
from .policy import RetryPolicy
from .store import canonical_socket
from .transport import (
    DEFERRED_BUSY,
    DISPATCHED,
    HELD_UNCERTAIN,
    INBOX_ONLY,
    QUEUED,
    SENDING,
    WITHHELD_PRE_SEND,
    classify_operation_receipt,
)

VERSION = "relay-supervisor-channel/1"

# The one state the transport vocabulary has no word for, because the transport cannot answer
# it: the recipient said it read this, and the host agreed the turn was real.
READ = "read"
CLAIMABLE = (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND)
# What the transport calls a message that reached somewhere. inbox_only is included for the
# reason ack includes it: it is stored rather than woken, and a recipient can still read it.
DELIVERED = (DISPATCHED, INBOX_ONLY)

# What a readback established about the turn it names.
HOST_READ = "host_read"
NO_HOST = "unverified_turn"
TURN_NOT_FOUND = "turn_not_found"
TURN_PREDATES_SEND = "turn_predates_send"
# The turn is real and the bytes are not established where the recipient reads. A separate
# word from the three above, because those are answers about the TURN and this one is about
# the MESSAGE: a genuine turn on the right thread was named, and nothing independent of our
# own receipt places this attempt's bytes in a turn the host names.
TRANSCRIPT_UNCONFIRMED = "transcript_unconfirmed"
# The readback names the turn the send itself opened, and the delivered bytes are not in that
# turn. Those two cannot both be true of one message: what the token is IN is where the
# message landed, so a claim to have read it in the turn it landed in has to be the same turn.
TRANSCRIPT_TURN_MISMATCH = "transcript_turn_mismatch"

# And which turn the readback names, which is what says how much it is worth. The relay knows
# the id of the turn its own send opened, so a readback from that turn rests on nothing the
# sender could not have produced alone. It is still the ordinary case, because the message IS
# what wakes the supervisor. Recording which one was named is the honest middle between refusing
# the ordinary case and calling it more than it is.
RELAY_OPENED = "relay_opened"
RECIPIENT_OPENED = "recipient_opened"
ORIGIN_UNKNOWN = "unknown"

PROJECT = "project"
INITIATIVE = "initiative"
# How far back the recipient transcript is scanned for the bytes this send froze. The bound
# reconciliation uses, and the answer carries whether the scan was exhausted, because a
# truncated scan is inconclusive and never proof of absence.
TRANSCRIPT_SCAN = 200
NEWLINE = chr(10)
# What any readback answer says it establishes, and what it does not.
READBACK_LIMITS = (
    "a verified readback says this attempt's request id is in a named turn of the recipient's"
    " thread - the turn it names, or one that turn follows - and that the named turn did not"
    " certainly begin before this attempt's transport started. It does not say that turn"
    " answered or who computed the proof: the proof is two identifiers this store holds, and"
    " when the named turn is the one the send opened (turnOrigin relay_opened) no act of the"
    " recipient's is needed at all, so it shows arrival rather than reading. The turn and the"
    " transcript are read by separate host calls with no shared snapshot, which is sound for"
    " an append-only history and blind to one rewritten between them")
# The journal kind a handover's re-address writes. A busy deferral counts from the latest one,
# because a busy former supervisor is not a reason to hold the report its successor is owed.
READDRESSED = "supervisor_message_readdressed"
# What the reader of a message fills in. Each is one shell word, so the rendered line splits
# into the argv it looks like, and none of them is a value this relay could know.
SOCKET_PLACEHOLDER = "YOUR_RELAY_SOCKET"
TURN_PLACEHOLDER = "YOUR_TURN_ID"
PROOF_PLACEHOLDER = "YOUR_PROOF"
# What a readback establishes, by the origin of the turn it names. Printed beside every "read"
# this channel reports, because the state's name is shorter than what it proves: a readback of
# the turn the send opened needs nothing the recipient did.
ESTABLISHES = {
    RELAY_OPENED: "arrival only: the turn this send opened holds this attempt's request id,"
                  " and no act of the recipient's was needed",
    RECIPIENT_OPENED: "a turn the recipient's thread opened after the send, and this attempt's"
                      " request id in its transcript - not who wrote the answer or that"
                      " anybody read or acted",
}
NOT_ESTABLISHED = "nothing: this readback did not verify"


def _establishes(verified, origin):
    if verified != HOST_READ:
        return NOT_ESTABLISHED
    return ESTABLISHES.get(origin, "a verified turn whose origin the host did not establish")


def _command(*argv) -> str:
    """One command line from its arguments, each quoted for a POSIX shell.

    Every command this channel writes into a message goes through here. Concatenating the
    arguments rendered a workspace such as /tmp/My Project as two of them, so a pointer that
    looked runnable ran with the wrong path; shlex.split of the result gives back exactly
    these arguments, which is the property a reader relies on.
    """
    return " ".join(shlex.quote(str(one)) for one in argv)


def _addressed_as(row, resolution):
    """Whether a staged row still names the hierarchy that is live now."""
    return (row["sender_task_id"] == resolution["sender"]
            and row["recipient_task_id"] == resolution["recipient"]
            and row["project_key"] == resolution["projectKey"])


def _host_time(value):
    """A host timestamp in seconds, or None when it is not a finite number.

    Every chronology this channel checks reads its times through here or _iso_time, because
    the comparison that used to do it answered False for a value it could not read - so a
    start of "not-a-timestamp" or NaN counted as "not earlier" and verified. An unreadable
    time is not established, and the callers treat that as not verified.
    """
    if value is None or isinstance(value, bool):
        return None
    try:
        seconds = float(value)
    except (TypeError, ValueError):
        return None
    return seconds if math.isfinite(seconds) else None


def _iso_time(value):
    """One of this store's own ISO instants in seconds, or None when it cannot be read."""
    try:
        seconds = datetime.fromisoformat(value).timestamp()
    except (TypeError, ValueError):
        return None
    return seconds if math.isfinite(seconds) else None


def _began_before(first, second):
    """Whether one host start is certainly earlier than another: True, False, or None.

    None when either is not a time, so the caller treats "not established" as not verified
    rather than as "not earlier", which is the side an unknown must not fall on.
    """
    first, second = _host_time(first), _host_time(second)
    if first is None or second is None:
        return None
    return first + TURN_START_PRECISION_SECONDS <= second


def _nothing_owed(obligation, decided):
    """The refusal for an obligation select() says produces no report, decided under the lock."""
    return DeliveryRefused(
        RefusalReason.NOT_CLAIMABLE,
        "nothing is owed upward for obligation " + repr(obligation["obligationId"])
        + ": " + str(decided["reason"]) + ", decided under the staging write lock."
        " The obligation is preserved either way; what is refused is producing a second"
        " report about a fact somebody has already reported or the supervisor can already"
        " read for itself",
    )


# compose() reads the work report itself unless the caller hands it the one it already read.
_UNREAD = object()


def _reading_disagrees(obligation, reading):
    """What in an omission's reading contradicts the obligation it is staged with, or None.

    Complete selectors are not enough: a reading for another relationship or another turn has
    all of them, and staging with it put one relationship's evidence pointer inside another's
    report. Checked before anything is composed or recorded.

    And the named checks are not enough either. The obligation id is keyed on the
    relationship and the turn, so a reading from another generation of the same turn derives
    the SAME id and passes all four, and then a generation-1 packet carried generation-2
    evidence. So the obligation this reading raises is derived again and compared field by
    field with the one handed in, which also refuses an obligation a caller altered.
    """
    if reading.get("schema") != supervision.OBSERVATION_SCHEMA:
        return ("its schema is " + repr(reading.get("schema")) + ", not "
                + repr(supervision.OBSERVATION_SCHEMA))
    if reading.get("reportingState") != supervision.OBSERVED_OMISSION:
        return ("it reports " + repr(reading.get("reportingState")) + ", not "
                + repr(supervision.OBSERVED_OMISSION))
    if reading.get("relationshipId") != obligation["relationId"]:
        return ("it names relationship " + repr(reading.get("relationshipId"))
                + " and the obligation is for " + repr(obligation["relationId"]))
    turn = (reading.get("selectors") or {}).get("turn")
    if turn != obligation["subject"]:
        return ("its selectors name turn " + repr(turn) + " and the obligation is about turn "
                + repr(obligation["subject"]))
    raised = supervision.from_observation(reading)
    if raised is None:
        return "it raises no obligation at all"
    drift = _obligation_drift(obligation, raised)
    if drift:
        return "the obligation's " + ", ".join(drift) + " differ from what it raises"
    return None


# Every field supervision._obligation derives, which is everything compose() or the journal
# entry reads off an obligation. The annotations standing_for adds beside them - its decision,
# the relationship's status - are readings about the obligation, not the obligation.
_OBLIGATION_FIELDS = ("obligationId", "kind", "relationId", "subject", "executionGeneration",
                      "revisionHash", "issueKey", "basis", "detail")


def _obligation_drift(handed, derived):
    """The obligation fields a caller's copy does not share with the one derived now."""
    return [name for name in _OBLIGATION_FIELDS if handed.get(name) != derived.get(name)]


def _reading_key(reading):
    """What one omission reading contributes to a packet: the obligation and the pointer."""
    raised = supervision.from_observation(reading) or {}
    return json.dumps({"obligation": {name: raised.get(name) for name in _OBLIGATION_FIELDS},
                       "selectors": _selectors(reading)}, sort_keys=True, default=str)


def _frozen_reading_key(row):
    """_reading_key of the reading a staged row froze, or None when it froze none."""
    frozen = row["reading"]
    return _reading_key(json.loads(frozen)) if frozen else None


def _recheck_line(reading):
    """The reporting-show line that re-reads an omission's turn NOW, from its own selectors.

    Its store selection is the reading's own, which is the one that produced it. Rendered whole
    because reporting-show refuses without every selector, and quoted because the selectors
    are paths and names a caller chose.
    """
    selectors = _selectors(reading)
    if selectors is None:
        return None
    return _command("codex-session-relay", "--state", selectors["state"],
                    "reporting-show", "--marker-root", selectors["markerRoot"],
                    "--workspace", selectors["workspace"],
                    "--assignment", selectors["assignment"],
                    "--session", selectors["session"], "--turn", selectors["turn"])


def _submission_of(report):
    return report["submissionNo"] if report else None


def _same_hierarchy(first, second):
    """Whether two resolutions name the same sender, recipient and project."""
    return all(first[key] == second[key] for key in ("sender", "recipient", "projectKey"))


def _hierarchy_moved(read, live):
    """The refusal for a hierarchy that changed between a caller's reading and its write."""
    return DeliveryRefused(
        RefusalReason.RELATION_OWNER_DRIFT,
        "the hierarchy moved while this was being decided: it was read as "
        + repr(read["sender"]) + " reporting to " + repr(read["recipient"])
        + ", and under the write lock it is " + repr(live["sender"]) + " reporting to "
        + repr(live["recipient"]) + ". Nothing was written; staging again addresses the report"
        " to the live supervisor",
    )


class _NotClaimable(Exception):
    """Nothing to claim, for any reason. The caller does nothing and reports nothing."""


class _Paced(_NotClaimable):
    """A claim refused on the recipient's shared send budget, and on nothing else.

    attempt() defers it by the same gap the preflight uses, so a report refused inside the claim
    is rescheduled rather than left eligible for a retry the same budget refuses again.
    """


class SupervisorChannel:
    """Staging, sending and reading back what a parent owes the level above."""

    def __init__(self, store, registry, linkage, clock, *, policy=None, settings=None,
                 require_lifecycle_evidence: bool = True, state_directory=None,
                 socket_path=None):
        self.store = store
        self.registry = registry
        # Required rather than optional. delivery keeps a frozen relationship row it can fall
        # back on; this channel has none, because who supervises a project lives nowhere else.
        self.linkage = linkage
        self.clock = clock
        self.policy = policy or RetryPolicy()
        self.require_lifecycle_evidence = require_lifecycle_evidence
        self._settings = settings
        # The store selection every line this channel writes for a later step reproduces. A
        # later command reaches this store only if it selects it the same way, and a bare
        # --socket selects the socket-scoped default - which is not this store whenever it was
        # chosen with --state or the environment override. So each rendered line names the
        # directory itself, and the readback line names the socket the send went through,
        # which is the host the recipient's thread is on. Without a socket - a library caller,
        # a test - that one word stays a placeholder the reader has to fill.
        self.state_directory = (str(state_directory) if state_directory
                                else os.path.dirname(os.path.abspath(str(store.path))))
        # Canonical, as the store records it: a relative spelling would resolve against the
        # reader's working directory rather than the sender's.
        self.socket_path = canonical_socket(socket_path) if socket_path else None

    def _command_line(self, *argv, socket=False) -> str:
        """A codex-session-relay line that selects THIS store, and this host when asked to."""
        head = ["codex-session-relay", "--state", self.state_directory]
        if socket:
            head += ["--socket", self.socket_path or SOCKET_PLACEHOLDER]
        return _command(*head, *argv)

    # ------------------------------------------------------- who may send, and to whom

    def resolve(self, relationship_id) -> dict:
        """The live parent of the project, and the live supervisor above it.

        Read from the hierarchy rather than from a frozen row - the rule
        delivery.resolve_recipient already applies to a completion, for the same reason: an
        assignment registered before a handover names the owner that stepped down.

        The three answers stay three answers. An unreadable store has said nothing about the
        owner, a contested one has said two things, and neither becomes a recipient.
        """
        reading = self.linkage.up(relationship_id=relationship_id)
        if not reading.get("readable", False):
            raise DeliveryRefused(
                RefusalReason.RELATION_UNREADABLE,
                "the linkage could not be read for relationship " + repr(relationship_id)
                + ", so who the level above is is unknown; nothing is staged and the"
                " obligation stays exactly where it was",
            )
        if reading.get("state") == "ambiguous":
            raise DeliveryRefused(
                RefusalReason.DUPLICATE_SCOPE_OWNER,
                "the linkage reports more than one candidate above relationship "
                + repr(relationship_id) + "; this sender will not choose between them: "
                + repr(reading.get("contention")),
            )
        contention = [one for one in (reading.get("contention") or []) if one.get("contention")]
        if contention:
            # Only the walk's own live findings. linkage folds retained linkage_conflicts rows
            # into the same list and nothing ever deletes those, so refusing on them would let
            # one historical refusal silence a project for good. A walk finding carries a
            # "contention" key; an audit row carries "reason" and no "contention".
            drifting = any(one.get("contention") == "owner_drift" for one in contention)
            raise DeliveryRefused(
                RefusalReason.RELATION_OWNER_DRIFT if drifting else RefusalReason.LINK_CONFLICT,
                "the hierarchy above relationship " + repr(relationship_id)
                + " is not settled: " + repr(contention) + ". A report waits for it to settle"
                " rather than being filed with whichever candidate happens to match",
            )
        levels = {one.get("scopeKind"): one for one in reading.get("levels") or []}
        project = self._owner(levels.get(PROJECT))
        supervisor = self._owner(levels.get(INITIATIVE))
        if project is None:
            raise DeliveryRefused(
                RefusalReason.UNREGISTERED_SCOPE,
                "relationship " + repr(relationship_id) + " has no live project owner, so"
                " there is nobody whose report this would be; gaps "
                + repr(reading.get("gaps")),
            )
        if supervisor is None:
            raise DeliveryRefused(
                RefusalReason.UNREGISTERED_SCOPE,
                "no initiative supervises the project above relationship "
                + repr(relationship_id) + ", so there is nobody to report to; gaps "
                + repr(reading.get("gaps")) + ". The obligation stays standing, which is the"
                " difference between having nowhere to send a report and not owing one",
            )
        return {
            "sender": project["taskId"],
            "recipient": supervisor["taskId"],
            "projectKey": levels[PROJECT].get("scopeKey"),
            "initiativeKey": levels[INITIATIVE].get("scopeKey"),
            "source": "linkage",
        }

    @staticmethod
    def _owner(level):
        if not level or level.get("owner") is None:
            return None
        owner = level["owner"]
        return owner if isinstance(owner, dict) else {"taskId": owner}

    def _issue_of(self, relationship_id):
        row = self.store.one(
            "SELECT issue_key FROM relationships WHERE relationship_id = ?", (relationship_id,))
        return row["issue_key"] if row is not None else None

    def _hierarchy_in(self, db, message_id):
        """Whether the hierarchy a staged row names is still the live one, asked in a write.

        Returns (the live resolution, None) when it is, else (None, the refusal saying what
        moved). Called with the caller's write transaction open, so the answer is the store as
        that write will commit it.

        The question is put to resolve(), the one function that decides who a report is for,
        rather than to a predicate written beside it. A predicate over the two owner bindings
        missed everything else that decides the answer: an archived relationship releases the
        issue edge resolve() walks, a project can be re-linked under another initiative, and
        an edge can drift or be contested - and each of those sent a report resolve() would
        have refused, because the copy of the rule knew only part of it. Asking the rule
        itself inside the write is what stage() and _readdress already do.
        """
        row = db.execute(
            "SELECT relationship_id, sender_task_id, recipient_task_id, project_key"
            "  FROM supervisor_messages WHERE message_id = ?", (message_id,)).fetchone()
        if row is None:
            return None, DeliveryRefused(
                RefusalReason.NOT_CLAIMABLE, "no supervisor message is staged as "
                + repr(message_id))
        try:
            live = self.resolve(row["relationship_id"])
        except DeliveryRefused as refusal:
            return None, refusal
        if not _addressed_as(row, live):
            return None, DeliveryRefused(
                RefusalReason.RELATION_OWNER_DRIFT,
                "message " + repr(message_id) + " names " + repr(row["sender_task_id"])
                + " reporting to " + repr(row["recipient_task_id"]) + " for project "
                + repr(row["project_key"]) + ", and the linkage now says "
                + repr(live["sender"]) + " reports to " + repr(live["recipient"])
                + " for project " + repr(live["projectKey"]))
        return live, None

    # ------------------------------------------------------------------ what is staged

    def find(self, message_id):
        return self.store.one(
            "SELECT * FROM supervisor_messages WHERE message_id = ?", (message_id,))

    def get(self, message_id):
        row = self.find(message_id)
        if row is None:
            raise DeliveryRefused(
                RefusalReason.NOT_CLAIMABLE,
                "no supervisor message is staged as " + repr(message_id))
        return row

    def stage(self, obligation, *, expect_recipient=None, reading=None) -> dict:
        """Freeze what this obligation owes upward, before anything is sent.

        Idempotent on the message id, which is derived from the fact rather than from the
        moment: staging the same obligation after a restart, a compaction or a service
        replacement finds the first row instead of producing a second report.

        A caller may name the recipient it believes in. A disagreement with the linkage IS the
        finding and is refused: substituting the live owner for the named one would file a
        report with whoever asked, and substituting the named one for the live owner would file
        it with a task that no longer supervises anything.

        An omission carries its READING, and is refused without one. The obligation names a
        relationship and a turn; the command that produced it needs a state directory, a marker
        root, a workspace, an assignment and a session as well, and none of those is derivable
        from the obligation. Composing the pointer without them produced an evidence line that
        looked like a command and could not be run, which is worse than admitting there is no
        pointer - so the reading is required where the pointer needs it.
        """
        if obligation["kind"] == supervision.UNREPORTED and _selectors(reading) is None:
            raise DeliveryRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "an omission is staged with the reporting-observation/1 reading it came from:"
                " the obligation names a relationship and a turn, and the command that found"
                " it also needs the state directory, marker root, workspace, assignment and"
                " session. Without them the report would carry an evidence line nobody can"
                " follow",
            )
        if obligation["kind"] == supervision.UNREPORTED:
            disagrees = _reading_disagrees(obligation, reading)
            if disagrees is not None:
                raise DeliveryRefused(
                    RefusalReason.CONTRADICTORY_OBSERVATION,
                    "the reading staged with this omission contradicts it: " + disagrees
                    + ". Its selectors would become the evidence pointer of a report about"
                    " something else, so nothing was composed or recorded",
                )
        resolution = self.resolve(obligation["relationId"])
        if expect_recipient is not None and expect_recipient != resolution["recipient"]:
            raise DeliveryRefused(
                RefusalReason.RECIPIENT_NOT_AUTHORIZED,
                "the caller named " + repr(expect_recipient) + " and the linkage says project "
                + repr(resolution["projectKey"]) + " is supervised by "
                + repr(resolution["recipient"]) + "; a disagreement about who the level above"
                " is is the finding, not something to resolve by picking one",
            )
        from .report import read as read_work_report

        # Read once, composed from, and read again under the lock below: the packet freezes
        # this report's artifact and decision, and its evidence reads this event's report.
        event_id = (obligation.get("basis") or {}).get("eventId")
        report = read_work_report(self.store, event_id) if event_id else None
        if event_id:
            # The obligation handed in has to be the one this event raises, field for field.
            # Comparing its id alone let a caller's copy with another generation or issue
            # through, and the packet and the journal entry are both composed from the copy.
            raised = supervision.from_event(self.store, event_id, report)
            drift = (["obligationId"] if raised is None
                     else _obligation_drift(obligation, raised))
            if drift:
                raise DeliveryRefused(
                    RefusalReason.CONTRADICTORY_OBSERVATION,
                    "the obligation handed in is not the one event " + repr(event_id)
                    + " raises: its " + ", ".join(drift) + " differ. The packet and the"
                    " journal entry would both be composed from it, so nothing was composed"
                    " or recorded; stage the obligation the event raises")
        observed_at = self.clock.iso()
        packet = self.compose(obligation, resolution=resolution, reading=reading,
                              observed_at=observed_at, report=report)
        message_id = packet["envelope"]["messageId"]
        at = self.clock.iso()
        staged = False
        existing = None
        with self.store.composing() as db:
            # Both questions are asked HERE, under the write lock, and nowhere before it. A
            # reading taken earlier can be overtaken twice over: a supervisor-report-recorded
            # committing in between left the insert standing beside a journal entry that
            # already said the report existed, and a stage from the former hierarchy
            # committing in between made this caller refuse or return that row instead of
            # re-addressing it, so the successor was never told.
            # And the hierarchy itself is read again: the packet was composed for the
            # resolution taken before the lock, and a handover committing since would have this
            # caller insert a message for a supervisor who has stepped down.
            live = self.resolve(obligation["relationId"])
            if not _same_hierarchy(live, resolution):
                raise _hierarchy_moved(resolution, live)
            # And the work report is still the one the packet was composed from, WHOLE, and
            # still raises this obligation field for field. A correction committing between
            # the read above and this lock left a packet naming the old artifact while its
            # evidence read the new one - and comparing the submission number alone missed a
            # correction made in place under the same number. Once this commits,
            # report.record refuses to change the report at all.
            current = None
            if event_id:
                current = read_work_report(self.store, event_id)
                fresh = supervision.from_event(self.store, event_id, current)
                if (current != report or fresh is None
                        or _obligation_drift(obligation, fresh)):
                    raise DeliveryRefused(
                        RefusalReason.SUPERSEDED_REVISION,
                        "the work report for event " + repr(event_id) + " changed while this"
                        " was being staged (submission " + repr(_submission_of(report))
                        + " was read, " + repr(_submission_of(current)) + " stands now), so"
                        " the packet would describe a report its evidence no longer shows."
                        " Nothing was written; stage it again from the report that stands")
            # And the bytes themselves. Composed again from what the store says under this
            # lock, with the same observation time, and required to be the packet about to be
            # frozen - so anything compose() reads that the checks above do not name cannot
            # drift between the two either.
            if self.compose(obligation, resolution=live, reading=reading,
                            observed_at=observed_at, report=current) != packet:
                raise DeliveryRefused(
                    RefusalReason.SUPERSEDED_REVISION,
                    "the packet composed under the staging lock differs from the one composed"
                    " before it, so something it is composed from moved in between. Nothing"
                    " was written; stage it again")
            existing = db.execute("SELECT * FROM supervisor_messages WHERE message_id = ?",
                                  (message_id,)).fetchone()
            if (existing is not None and obligation["kind"] == supervision.UNREPORTED
                    and _frozen_reading_key(existing) != _reading_key(reading)):
                # One omission, one reading. The row froze the reading its packet was composed
                # from and its evidence prints; returning it as this caller's, or re-addressing
                # it with a packet composed from another reading, would put two observations
                # behind one message.
                raise DeliveryRefused(
                    RefusalReason.CONTRADICTORY_OBSERVATION,
                    "this omission is already staged as " + repr(message_id) + " from another"
                    " reading of it, which disagrees with this one about what it is or where"
                    " it can be read. The staged message keeps the reading it froze; nothing"
                    " was written")
            if existing is None:
                decided = supervision.select(self.store, obligation, recipient=None)
                if not decided["report"]:
                    raise _nothing_owed(obligation, decided)
                cursor = db.execute(
                    "INSERT OR IGNORE INTO supervisor_messages (message_id, obligation_id,"
                    " obligation_kind, relationship_id, project_key, purpose, kind,"
                    " sender_task_id, recipient_task_id, subject, packet, state,"
                    " attempt_count, next_eligible_at, staged_at, updated_at, event_id,"
                    " submission_no, reading) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0,NULL,?,?,?,?,?)",
                    (message_id, obligation["obligationId"], obligation["kind"],
                     obligation["relationId"], resolution["projectKey"],
                     packet["envelope"]["purpose"], packet["envelope"]["kind"],
                     resolution["sender"], resolution["recipient"], obligation["subject"],
                     json.dumps(packet, ensure_ascii=False, sort_keys=True), QUEUED, at, at,
                     event_id, _submission_of(report),
                     json.dumps(reading, ensure_ascii=False, sort_keys=True)
                     if obligation["kind"] == supervision.UNREPORTED else None),
                )
                staged = cursor.rowcount == 1
                if staged:
                    # In the SAME transaction as the row. A report that exists and is not
                    # recorded would be produced again by the next reading of the same fact,
                    # which is the duplicate wake the journal entry exists to prevent; and a
                    # record with no row would suppress a report nobody can send.
                    supervision.record_report(
                        self.store, obligation, at=at, messageId=message_id,
                        note="staged on the supervisor channel")
        if existing is not None:
            if _addressed_as(existing, resolution):
                return {"schema": VERSION, "staged": False, "messageId": message_id,
                        "reason": "this fact is already staged; one obligation is one message",
                        "message": dict(existing)}
            # The id is the FACT's and the endpoints are the hierarchy's, so a handover moves
            # the second from under the first. Returning the row as it stands is what left a
            # report staged for a supervisor who had stepped down: attempt() refuses it as
            # drift, and the journal keeps a replacement from being produced. _readdress
            # decides again inside its own write, so a claim committing after this read wins.
            return self._readdress(existing, packet, resolution)
        return {"schema": VERSION, "staged": staged, "messageId": message_id,
                "reason": "staged" if staged else "another caller staged this fact first",
                "message": dict(self.get(message_id)),
                "recipient": resolution["recipient"], "sender": resolution["sender"]}

    def _readdress(self, row, packet, resolution) -> dict:
        """A staged message whose hierarchy moved before anything was sent.

        Nothing sent is the one state in which moving a message is safe, because nothing
        outside this store has seen it: the bytes are rendered inside the claim, against the
        endpoints on the row at that moment, and an attempt that recorded sendAttempted no
        and retry-safe put them nowhere - a transport that refused before sending, or a
        transport start that found the hierarchy moved. So the row is re-addressed IN PLACE -
        same id, same supervisor_report entry, one obligation still one message - and the
        condition is a predicate inside the write rather than a check before it, so a claim
        that commits first wins and this becomes a refusal instead of a second recipient.

        What the former recipient's state decided goes with it. A backoff, a lifecycle recheck
        and a busy cap were all bounds about THAT task, and carrying them over would have the
        new supervisor inherit a wait it never caused; the deferral count restarts for the
        same reason, from this journal entry.

        Anything with an attempt that may have sent stays frozen. Those bytes may have reached
        somebody, possibly as a wake, and moving the row would leave its attempts describing a
        recipient they were never sent to. The drift is refused by name, which is how it gets
        reported.
        """
        message_id = row["message_id"]
        was = {"sender": row["sender_task_id"], "recipient": row["recipient_task_id"],
               "projectKey": row["project_key"]}
        now_is = {"sender": resolution["sender"], "recipient": resolution["recipient"],
                  "projectKey": resolution["projectKey"]}
        at = self.clock.iso()
        with self.store.composing() as db:
            # Re-resolved under this write too. A caller holding a reading from before a
            # handover would otherwise move a row a later caller had already addressed to the
            # successor back to the supervisor who stepped down.
            live = self.resolve(row["relationship_id"])
            if not _same_hierarchy(live, resolution):
                raise _hierarchy_moved(resolution, live)
            cursor = db.execute(
                "UPDATE supervisor_messages SET sender_task_id = ?, recipient_task_id = ?,"
                " project_key = ?, packet = ?, state = ?, next_eligible_at = NULL,"
                " hold_reason = NULL, updated_at = ?"
                " WHERE message_id = ? AND sender_task_id = ? AND recipient_task_id = ?"
                "   AND project_key IS ? AND state IN (?,?,?)"
                "   AND NOT EXISTS (SELECT 1 FROM supervisor_attempts a"
                "                    WHERE a.message_id = supervisor_messages.message_id"
                "                      AND (a.send_attempted <> 'no' OR a.retry_safe = 0))",
                (now_is["sender"], now_is["recipient"], now_is["projectKey"],
                 json.dumps(packet, ensure_ascii=False, sort_keys=True), QUEUED, at,
                 message_id, was["sender"], was["recipient"], was["projectKey"],
                 QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND),
            )
            moved = cursor.rowcount == 1
            if moved:
                self.store.journal(
                    READDRESSED, message_id,
                    {"from": was, "to": now_is, "releasedHold": row["hold_reason"],
                     "releasedState": row["state"],
                     "reason": "the hierarchy moved before anything was sent"}, at=at)
        current = self.get(message_id)
        if moved:
            return {"schema": VERSION, "staged": False, "readdressed": True,
                    "messageId": message_id, "from": was, "to": now_is,
                    "reason": "staged for a hierarchy that has since moved, and never"
                              " attempted, so it now goes to the live supervisor instead",
                    "message": dict(current),
                    "recipient": now_is["recipient"], "sender": now_is["sender"]}
        if _addressed_as(current, resolution):
            return {"schema": VERSION, "staged": False, "readdressed": False,
                    "messageId": message_id,
                    "reason": "another caller re-addressed this message to the live"
                              " supervisor first",
                    "message": dict(current),
                    "recipient": now_is["recipient"], "sender": now_is["sender"]}
        raise DeliveryRefused(
            RefusalReason.RELATION_OWNER_DRIFT,
            "message " + repr(message_id) + " was staged from " + repr(current["sender_task_id"])
            + " to " + repr(current["recipient_task_id"]) + " and has "
            + str(current["attempt_count"]) + " attempt(s), state "
            + repr(current["state"]) + "; the linkage now says " + repr(now_is["sender"])
            + " reports to " + repr(now_is["recipient"]) + ". A message an attempt may have"
            " sent is never re-addressed, because its attempts would then describe a recipient"
            " they were never sent to - it went to the supervisor who was live when its"
            " transport started - so " + repr(now_is["recipient"]) + " has not been told by"
            " this channel, and the obligation stands until the Linear record confirms it",
        )

    def stage_standing(self, project_key, *, observations=()) -> dict:
        """Everything a project still owes upward, staged in one call.

        One command rather than one decision per event, which is what makes the reporting
        obligation something a parent discharges rather than something it has to remember. What
        is refused is reported beside what was staged: a project where one relationship has no
        supervisor is not a project where nothing can be reported.
        """
        standing = supervision.standing_for(
            self.store, self.linkage, project_key, observations=observations)
        staged, refused = [], []
        # The readings are indexed by the obligation each one RAISES, and every distinct
        # reading of one obligation is kept. Indexing by relationship and turn and keeping the
        # last one let standing_for derive the obligation from the first reading while stage()
        # received another - a generation-1 packet carrying generation-2 evidence - because
        # the obligation id does not include the generation. Two readings of one omission
        # that disagree about what it is or where it can be read are a contradiction, and
        # this caller does not pick one.
        readings = {}
        for one in observations:
            raised = supervision.from_observation(one)
            if raised is not None and _selectors(one) is not None:
                readings.setdefault(raised["obligationId"], {}).setdefault(
                    _reading_key(one), one)
        for obligation in standing["standing"]:
            found = list(readings.get(obligation["obligationId"], {}).values())
            if len(found) > 1:
                refused.append({
                    "obligationId": obligation["obligationId"],
                    "kind": obligation["kind"],
                    "reason": RefusalReason.CONTRADICTORY_OBSERVATION.value,
                    "detail": str(len(found)) + " readings of this omission disagree about"
                              " what it is or where it can be read, and a report carries"
                              " exactly one; nothing was staged for it",
                })
                continue
            try:
                staged.append(self.stage(obligation, reading=found[0] if found else None))
            except DeliveryRefused as refusal:
                refused.append({
                    "obligationId": obligation["obligationId"],
                    "kind": obligation["kind"],
                    "reason": refusal.reason.value if refusal.reason else None,
                    "detail": refusal.detail,
                })
        return {"schema": VERSION, "projectKey": project_key, "staged": staged,
                "refused": refused, "gaps": standing["gaps"],
                "limits": "staging is not sending and sending is not reading. Each message"
                          " here has to be sent and read back before anything says it arrived,"
                          " and a readback shows arrival rather than that a supervisor read it"}

    # ------------------------------------------------------------------ what it carries

    def compose(self, obligation, *, resolution, reading=None, observed_at=None,
                report=_UNREAD) -> dict:
        """The relay-packet/1 this obligation travels as.

        Composed through packets.compose rather than assembled here, so the occasion is checked
        against what its own purpose cannot do without and this module cannot drift from the
        table that decides it.
        """
        from .report import read as read_work_report

        purpose = supervision.PURPOSE[obligation["kind"]]
        issue = obligation.get("issueKey") or self._issue_of(obligation["relationId"])
        event_id = (obligation.get("basis") or {}).get("eventId")
        if report is _UNREAD:
            report = read_work_report(self.store, event_id) if event_id else None
        decision = None
        if envelope.kind_of(envelope.PARENT_TO_SUPERVISOR, purpose) == envelope.DECISION:
            decision = self._decision(obligation)
        # The id the envelope will derive, known before composing: an omission's evidence is
        # this message's own record, so the pointer has to name it.
        message_id = envelope.message_id(
            direction=envelope.PARENT_TO_SUPERVISOR, relation_id=obligation["relationId"],
            purpose=purpose, subject=obligation["subject"])
        packet = packets.compose(
            direction=envelope.PARENT_TO_SUPERVISOR,
            purpose=purpose,
            relation_id=obligation["relationId"],
            sender=resolution["sender"],
            recipient=resolution["recipient"],
            subject=obligation["subject"],
            issue=issue,
            generation=obligation.get("executionGeneration"),
            artifact=_artifact(report),
            evidence=self._evidence(obligation, resolution, reading, message_id),
            decision=decision,
            scope=self._scope(resolution, issue),
            basis=self._basis(obligation),
            observed_at=observed_at,
        )
        if packet["envelope"]["messageId"] != message_id:
            raise DeliveryRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the envelope derived message id " + repr(packet["envelope"]["messageId"])
                + " and the evidence was written for " + repr(message_id))
        return packet

    @staticmethod
    def _decision(obligation) -> str:
        """What the user is being asked, in the words the child used for it.

        A decision envelope cannot omit this, and it must not be filled with a restatement of
        its own kind: "a decision is needed" tells a reader nothing the word decision_request
        did not already tell them.
        """
        detail = (obligation.get("detail") or "").strip()
        return detail or ("the child stopped for a judgment the parent is not allowed to make"
                          " for the user, and recorded nothing further about it")

    def _evidence(self, obligation, resolution, reading=None, message_id=None) -> list:
        """Where the fact is readable, in a version that cannot change under the packet.

        An event's evidence is its work report, which report.record refuses to change once a
        message names the event. An omission's is the reading it was staged from, frozen on
        the message row and printed by supervisor-show: the command that produced the reading
        re-reads the turn NOW, and a report that reached the turn afterwards made it answer
        reported under a packet that says unreported. supervisor-show prints that command
        too, as a recheck, beside the frozen reading.

        Every line selects this store explicitly (see _command_line).
        """
        basis = obligation.get("basis") or {}
        event_id = basis.get("eventId")
        if event_id:
            return [self._command_line("show", "--event", event_id)]
        if _selectors(reading) is not None and message_id:
            return [self._command_line("supervisor-show", "--message", message_id)]
        return [self._command_line("supervisor-standing", "--project",
                                   resolution.get("projectKey"))]

    @staticmethod
    def _scope(resolution, issue):
        project = resolution.get("projectKey")
        if project and issue:
            return "project " + str(project) + ", issue " + str(issue)
        if project:
            return "project " + str(project)
        return envelope.absent(envelope.UNKNOWN, "no Linear scope was readable")

    @staticmethod
    def _basis(obligation):
        revision = obligation.get("revisionHash")
        if not revision:
            return None
        return ("generation " + str(obligation.get("executionGeneration")) + ", revision "
                + str(revision)[:12])

    def render(self, packet, request_id) -> str:
        """The bytes one attempt freezes. A function of the packet and the request id alone.

        The recipient's own turn id is absent, and that absence is what makes the readback
        worth having: a reply that quotes every delivered field back still cannot produce the
        proof. It is also unavoidable - the turn does not exist until these bytes arrive.
        """
        region = packet["envelope"]
        lines = ["[codex-session-relay] supervisor report", "requestId: " + request_id]
        lines += packets.packet_lines(packet)
        # What the recipient is asked for is what the readback can record: that this attempt
        # reached its thread, answered from a turn of its own. Asking it to "confirm you read
        # this" asked for an acknowledgement the check cannot establish - where the named turn
        # is the one this message opened, it needs nothing the recipient did at all.
        placeholders = ([SOCKET_PLACEHOLDER] if self.socket_path is None else []) + [
            TURN_PLACEHOLDER, PROOF_PLACEHOLDER]
        words = {SOCKET_PLACEHOLDER: SOCKET_PLACEHOLDER + " with your relay socket path,"
                                     " which these bytes do not know,",
                 TURN_PLACEHOLDER: TURN_PLACEHOLDER + " with your own turn id,",
                 PROOF_PLACEHOLDER: PROOF_PLACEHOLDER + " with the proof over it."}
        lines += [
            "",
            "To record that this reached your thread, run from inside a turn of your own:",
            "  " + self._command_line("supervisor-read", "--message", region["messageId"],
                                      "--turn", TURN_PLACEHOLDER, "--proof", PROOF_PLACEHOLDER,
                                      "--as", envelope.shown(region["recipient"]["taskId"]),
                                      socket=True),
            "",
            str(len(placeholders)) + " words on that line are yours to replace: "
            + " ".join(words[one] for one in placeholders),
            "Every other argument is filled in and quoted for a POSIX shell, including the",
            "--state that selects the store this report was staged in; --socket and --state",
            "are global and go BEFORE the subcommand, and --as is required.",
            "",
            "The proof is sha256(messageId|<your own turn id>). This message cannot contain",
            "that turn id, so quoting it back does not produce the proof - and that is all the",
            "proof rules out. Anyone holding the relay's store can compute it as well. A",
            "readback records that this attempt's request id is in your thread and that the",
            "turn you name is real there. Where that turn is the one this message opened, it",
            "records arrival and nothing you did. It never records that you read, agreed to",
            "or acted on anything.",
            "",
            "Full record: " + self._command_line("supervisor-show", "--message",
                                                 region["messageId"]),
        ]
        return NEWLINE.join(lines)

    # ----------------------------------------------------------------------- sending it

    def eligible(self, *, now, limit: int = 4) -> list:
        """The oldest staged messages that may be sent now, in the order they were staged.

        Ordered by staging rather than by anything about the recipient, so two facts reach the
        level above in the order they arose. This queue is read only by this class: the
        parent-child engine selects over deliveries by event id and cannot see these rows, and
        nothing here can claim one of those.
        """
        return self.store.all(
            "SELECT * FROM supervisor_messages"
            " WHERE state IN (?,?,?) AND hold_reason IS NULL"
            "   AND (next_eligible_at IS NULL OR next_eligible_at <= ?)"
            # message_id breaks a tie rather than rowid, because rowid is not in a SELECT *
            # and one ordering rule has to be readable from an ordinary row. The id is
            # derived from the fact, so the tie-break is arbitrary and stable.
            " ORDER BY staged_at, message_id LIMIT ?",
            (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, now, limit),
        )

    def attempt(self, message_id, adapter, *, now=None, owner: str = "relay"):
        """One attempt at one staged message, through the same host rules a delivery obeys.

        None means nothing was sent and nothing is wrong: a busy recipient, a backoff still
        running, a message already dispatched. A returned record is what the transport actually
        answered, classified by the same reader the parent-child path uses.
        """
        now = self.clock.now() if now is None else now
        row = self.get(message_id)
        row = self._recover_if_stranded(row, now)
        if row["hold_reason"]:
            return None
        if row["state"] not in CLAIMABLE:
            return None
        if row["next_eligible_at"] is not None and row["next_eligible_at"] > now:
            return None
        recipient = row["recipient_task_id"]
        older = self._older_claimable(row, now)
        if older is not None:
            raise DeliveryRefused(
                RefusalReason.NOT_CLAIMABLE,
                "message " + repr(older["message_id"]) + " was staged for "
                + repr(recipient) + " first and can be sent now, so this one waits. Two facts"
                " reach the level above in the order they arose, which is not a property an"
                " ordered selection can have on its own while any caller may name any row",
            )
        # Re-read immediately before the send, the way delivery re-checks authorization inside
        # attempt(): a handover committed since staging must not be delivered through.
        resolution = self.resolve(row["relationship_id"])
        if not _addressed_as(row, resolution):
            # Sending never re-addresses: which task a report is FOR is decided where it is
            # staged, so this refuses and says how the report recovers.
            recovery = (" It was never attempted, so staging it again re-addresses it to the"
                        " live supervisor" if self._nothing_sent(message_id) else
                        " Its bytes may have reached that task, so it stays addressed to it and"
                        " is not re-addressed")
            raise DeliveryRefused(
                RefusalReason.RELATION_OWNER_DRIFT,
                "this message was staged from " + repr(row["sender_task_id"]) + " to "
                + repr(recipient) + " and the linkage now says " + repr(resolution["sender"])
                + " reports to " + repr(resolution["recipient"]) + "; the hierarchy moved"
                " under a staged report, so it is held rather than sent to either." + recovery,
            )
        if self._rate_limited(recipient, now):
            self._reschedule(row, now + self.policy.min_send_interval_seconds)
            return None
        observation = observe(
            adapter, recipient, require_evidence=self.require_lifecycle_evidence)
        record_lifecycle(self.store, self.clock, observation)
        if observation.is_busy:
            self._defer_busy(row, now)
            return None
        if not observation.may_send:
            self._withhold(row, observation, now)
            return None
        # Established before anything is claimed or sent. The bridge refuses a send carrying no
        # settings, and a supervisor is a task like any other: what is preserved is what its
        # creation result recorded, never a host default.
        try:
            settings = self._settings_for(recipient, observation.runtime_status)
        except DeliveryRefused as refusal:
            self._withhold_settings(row, now, refusal)
            return None
        try:
            attempt_no, request_id, message = self._claim(
                message_id, now=now, owner=owner, recipient=recipient,
                resolution=resolution, sender=row["sender_task_id"])
        except _Paced:
            self._reschedule(row, now + self.policy.min_send_interval_seconds)
            return None
        except _NotClaimable:
            return None
        refused = self._start_transport(message_id, attempt_no, request_id, owner, recipient)
        if refused is not None:
            kind, detail = refused
            if kind == "moved":
                raise detail
            return None
        try:
            receipt = adapter.send_message(request_id, recipient, message, settings)
        except Exception as error:  # noqa: BLE001 - a transport fault is an unknown outcome
            receipt = {"requestId": request_id, "status": "outcome_unknown",
                       "error": type(error).__name__ + ": " + str(error)}
        facts = classify_operation_receipt(receipt)
        record = {
            "schema": VERSION,
            "requestId": request_id,
            "messageId": message_id,
            "attemptNo": attempt_no,
            "recipientTaskId": recipient,
            "deliveryState": facts.delivery_state,
            "sendAttempted": facts.send_attempted,
            "retrySafe": facts.retry_safe,
            "transportReceiptStatus": facts.transport_receipt_status,
            "failedOperation": facts.failed_operation,
            "turnId": facts.turn_id,
            "observedAt": self.clock.iso(),
        }
        self._settle(message_id, attempt_no, owner, request_id, facts, record, now)
        # Where the MESSAGE is, beside what the transport said about this attempt. They differ
        # when this send outlived its lease and was recovered as uncertain before its receipt
        # arrived: the receipt is recorded on the attempt, and the message stays where the
        # recovery put it.
        return {**record, "messageState": self.get(message_id)["state"]}

    def _start_transport(self, message_id, attempt_no, request_id, owner, recipient):
        """Stamp the instant the transport starts, under the lock that decides it may.

        A report goes to whoever supervises at its transport instant, and this write IS that
        instant. It stamps transport_started_at only while this caller's claim still holds the
        row and the hierarchy the message names is still the live one, and it is committed
        before the call - so a stamp that cannot be written raises before anything is sent.

        A handover committing between the claim and this write used to be sent through to the
        supervisor who had stepped down, and the report was then frozen with its attempt, so the
        successor was never told. Now that attempt is recorded as one that sent nothing, which
        is exactly true, and staging again re-addresses the message. A handover committing after
        this write finds a report already on its way to the supervisor who was live when it
        started, and that report stays with the task it went to.

        Whether the hierarchy still holds is asked of resolve() inside this write, the same
        question _claim asks. A store that is contested at this instant is answered the same
        way as a handover: the attempt is recorded as one that sent nothing and the message
        goes back to queued, so nothing is stranded by the refusal.

        Returns None when the transport may start, else ("lapsed", detail) or ("moved",
        the refusal to raise).
        """
        at = self.clock.iso()
        with self.store.transaction() as db:
            ours = db.execute(
                "SELECT 1 FROM supervisor_messages WHERE message_id = ? AND state = ?"
                "   AND attempt_count = ? AND lease_owner IS ? AND recipient_task_id = ?",
                (message_id, SENDING, attempt_no, owner, recipient)).fetchone()
            if ours is None:
                return ("lapsed", "this send's claim no longer holds the message, so its"
                                  " transport was not started")
            _live, moved = self._hierarchy_in(db, message_id)
            if moved is not None:
                record = {"requestId": request_id, "messageId": message_id,
                          "attemptNo": attempt_no, "deliveryState": WITHHELD_PRE_SEND,
                          "sendAttempted": "no", "retrySafe": True,
                          "reason": "the hierarchy moved between the claim and the transport",
                          "refusal": moved.reason.value if moved.reason else None,
                          "detail": moved.detail}
                db.execute(
                    "UPDATE supervisor_attempts SET state = ?, send_attempted = 'no',"
                    " retry_safe = 1, record = ?, observed_at = ? WHERE request_id = ?",
                    (WITHHELD_PRE_SEND, json.dumps(record, sort_keys=True), at, request_id))
                # Out of sending only by the claim that holds it: the same state, attempt and
                # lease owner the check above read, in the predicate of the write itself.
                db.execute(
                    "UPDATE supervisor_messages SET state = ?, next_eligible_at = NULL,"
                    " lease_owner = NULL, lease_until = NULL, updated_at = ?"
                    " WHERE message_id = ? AND state = ? AND attempt_count = ?"
                    "   AND lease_owner IS ?",
                    (QUEUED, at, message_id, SENDING, attempt_no, owner))
                self.store.journal(
                    "supervisor_message_withheld", message_id,
                    {"requestId": request_id,
                     "reason": "the hierarchy moved between the claim and the transport",
                     "refusal": moved.reason.value if moved.reason else None},
                    at=at)
                return ("moved", DeliveryRefused(
                    moved.reason or RefusalReason.RELATION_OWNER_DRIFT,
                    "the hierarchy this message names moved after its send was claimed and"
                    " before its transport started: " + str(moved.detail) + ". Nothing was"
                    " sent, and the attempt is recorded as sending nothing, so staging it"
                    " again re-addresses it to whoever the linkage names then"))
            db.execute(
                "UPDATE supervisor_attempts SET transport_started_at = ? WHERE request_id = ?",
                (at, request_id))
        return None

    def _nothing_sent(self, message_id):
        """Whether no attempt at this message can have put its bytes anywhere."""
        return self.store.one(
            "SELECT 1 FROM supervisor_attempts WHERE message_id = ?"
            "   AND (send_attempted <> 'no' OR retry_safe = 0) LIMIT 1",
            (message_id,)) is None


    def _recover_if_stranded(self, row, now):
        """A send whose process died between the claim and the receipt.

        The claim commits before the transport call, so a worker that dies in between leaves
        the message in sending with a lease nobody will ever settle. Left alone that is a
        strand rather than a hold: sending is not claimable, so no later pass touches it and
        the report can neither be sent nor read back.

        Expiry does not authorize a resend. Nothing observed what the transport did, and the
        rule the delivery path already follows holds here too - a lease prevents a second
        concurrent claimer and authorizes nothing on expiry. So the row moves to the state a
        classified uncertain receipt would have left it in: visible, holding, and never
        retried automatically, because a second send that lands is a second wake for one fact.
        """
        if row["state"] != SENDING:
            return row
        if row["lease_until"] is not None and row["lease_until"] > now:
            return row
        at = self.clock.iso()
        with self.store.transaction() as db:
            # The lease is re-checked here rather than trusted from the reading above: that
            # reading is taken outside the lock, and what decides a recovery is the row now.
            cursor = db.execute(
                "UPDATE supervisor_messages SET state = ?, lease_owner = NULL,"
                " lease_until = NULL, updated_at = ? WHERE message_id = ? AND state = ?"
                "   AND attempt_count = ? AND (lease_until IS NULL OR lease_until <= ?)",
                (HELD_UNCERTAIN, at, row["message_id"], SENDING, row["attempt_count"], now))
            if cursor.rowcount == 1:
                self.store.journal(
                    "supervisor_message_stranded", row["message_id"],
                    {"attemptNo": row["attempt_count"], "leaseOwner": row["lease_owner"],
                     "leaseUntil": row["lease_until"],
                     "reason": "the lease expired with no transport receipt, so what that"
                               " send did is unknown and is not repeated"}, at=at)
        return self.get(row["message_id"])

    def stranded(self, *, now=None) -> list:
        """Every message left mid-send by a process that did not come back.

        A read, so an operator can find them without sending anything. attempt() recovers the
        one it is called for; this is how the rest become visible.
        """
        now = self.clock.now() if now is None else now
        return [dict(row) for row in self.store.all(
            "SELECT * FROM supervisor_messages WHERE state = ?"
            "   AND (lease_until IS NULL OR lease_until <= ?) ORDER BY staged_at",
            (SENDING, now))]

    def _older_claimable(self, row, now):
        """An earlier message to the same recipient that could go now, or None.

        eligible() returns them oldest first, which orders the SELECTION and nothing else: any
        caller may name any message, so the ordering was a property of one code path rather
        than of the queue. Asked here and enforced again inside the claim, so the promise
        holds whoever is asking and however two callers interleave.
        """
        return self.store.one(
            "SELECT * FROM supervisor_messages"
            " WHERE recipient_task_id = ? AND message_id <> ?"
            # SENDING is in the set, because an older message IN FLIGHT is exactly when order
            # matters: letting a newer one past it is how the level above learns the second
            # fact first. A stranded row is excluded by its expired lease, so a worker that
            # died cannot block the queue behind it for ever.
            "   AND ((state IN (?,?,?) AND hold_reason IS NULL"
            "         AND (next_eligible_at IS NULL OR next_eligible_at <= ?))"
            "        OR (state = ? AND lease_until IS NOT NULL AND lease_until > ?))"
            "   AND (staged_at < ? OR (staged_at = ? AND message_id < ?))"
            " ORDER BY staged_at, message_id LIMIT 1",
            (row["recipient_task_id"], row["message_id"], QUEUED, DEFERRED_BUSY,
             WITHHELD_PRE_SEND, now, SENDING, now, row["staged_at"], row["staged_at"],
             row["message_id"]))

    def _settings_for(self, task_id, runtime_status=None):
        if self._settings is not None:
            return self._settings(task_id, runtime_status)
        return authorized_settings(self.store, task_id, runtime_status)

    def _claim(self, message_id, *, now, owner, recipient, resolution, sender=None):
        """Eligibility, the attempt number and the bytes, in one transaction.

        The bytes are rendered HERE, against the number this transaction just allocated, for
        the reason delivery renders inside its own claim: the request id is inside the bytes,
        so a render made before the allocation describes an attempt somebody else may have
        taken. Allocation, token and bytes commit together or not at all.

        The hierarchy is re-checked here too, by resolve() itself inside this write rather
        than by the reading taken before the host was read. That reading runs before the
        lifecycle reads, the settings gate and this claim, so a handover committing in that
        window left the staged row naming the former supervisor and the transport woke it -
        the live one never hearing the report. A preflight check can be raced; a question
        asked inside the write cannot, which is the closure delivery's own claim uses for a
        moved generation. It has to be resolve() and not a predicate beside it: the copy that
        compared only the two owner bindings let an archived relationship, a project moved to
        another initiative or a drifting edge through, each of which resolve() refuses.
        """
        with self.store.transaction() as db:
            live, moved = self._hierarchy_in(db, message_id)
            if moved is not None or not _same_hierarchy(live, resolution):
                # Moved since the caller read it, or no longer resolvable at all. Nothing is
                # claimed, and the next attempt's own reading says which.
                raise _NotClaimable()
            cursor = db.execute(
                "UPDATE supervisor_messages"
                "   SET state = ?, lease_owner = ?, lease_until = ?,"
                "       attempt_count = attempt_count + 1, updated_at = ?"
                " WHERE message_id = ?"
                "   AND state IN (?,?,?)"
                "   AND hold_reason IS NULL"
                "   AND (next_eligible_at IS NULL OR next_eligible_at <= ?)"
                # And the row still names the task this caller observed and will hand the
                # transport. A re-address committing between attempt()'s reads and this claim
                # left the row, and the bytes rendered from it, naming the successor while the
                # send went to the task observed before - so a claim for another endpoint is
                # no claim at all, and the next attempt reads the row as it now stands.
                "   AND recipient_task_id = ? AND (? IS NULL OR sender_task_id = ?)"
                # And the endpoints resolve() just named under this lock, so the row the
                # update moves is the row that answer was about.
                "   AND sender_task_id = ? AND recipient_task_id = ? AND project_key IS ?"
                # And nothing older to this recipient can go right now. Checked here as well
                # as before the host reads, because between those two a second caller can
                # stage or release an earlier message, and an ordering that two interleaved
                # callers can defeat is not an ordering.
                "   AND NOT EXISTS (SELECT 1 FROM supervisor_messages older"
                "                    WHERE older.recipient_task_id ="
                "                          supervisor_messages.recipient_task_id"
                "                      AND older.message_id <> supervisor_messages.message_id"
                "                      AND ((older.state IN (?,?,?)"
                "                            AND older.hold_reason IS NULL"
                "                            AND (older.next_eligible_at IS NULL"
                "                                 OR older.next_eligible_at <= ?))"
                "                           OR (older.state = ?"
                "                               AND older.lease_until IS NOT NULL"
                "                               AND older.lease_until > ?))"
                "                      AND (older.staged_at < supervisor_messages.staged_at"
                "                           OR (older.staged_at = supervisor_messages.staged_at"
                "                               AND older.message_id <"
                "                                   supervisor_messages.message_id)))",
                (SENDING, owner, now + self.policy.lease_seconds, self.clock.iso(),
                 message_id, QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, now,
                 recipient, sender, sender,
                 live["sender"], live["recipient"], live["projectKey"],
                 QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, now, SENDING, now),
            )
            if cursor.rowcount != 1:
                raise _NotClaimable()
            row = db.execute(
                "SELECT * FROM supervisor_messages WHERE message_id = ?", (message_id,)
            ).fetchone()
            attempt_no = row["attempt_count"]
            request_id = supervisor_request_id(message_id, attempt_no)
            # A request id keeps 12 hex characters of a 32-character message id, so two
            # messages CAN derive one. It is the primary key of the attempts table, so an
            # unchecked insert answers a collision with a raw IntegrityError out of the
            # driver - a host failure where a refusal belongs. Asked first, and named.
            clash = db.execute(
                "SELECT message_id FROM supervisor_attempts WHERE request_id = ?",
                (request_id,)).fetchone()
            if clash is not None and clash["message_id"] != message_id:
                raise DeliveryRefused(
                    RefusalReason.NOT_CLAIMABLE,
                    "request id " + repr(request_id) + " already belongs to message "
                    + repr(clash["message_id"]) + "; two message ids share the prefix this"
                    " id keeps, so this attempt cannot be told apart from that one",
                )
            message = self.render(json.loads(row["packet"]), request_id)
            at = self.clock.iso()
            db.execute(
                "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message,"
                " state, send_attempted, retry_safe, turn_id, record, sent_at, observed_at)"
                " VALUES (?,?,?,?,?,?,0,NULL,?,?,?)",
                (request_id, message_id, attempt_no, message, HELD_UNCERTAIN, "unknown",
                 json.dumps({"requestId": request_id, "messageId": message_id,
                             "attemptNo": attempt_no, "deliveryState": HELD_UNCERTAIN}),
                 at, at),
            )
            # Spent in the SAME transaction as the claim, against the recipient rather than
            # against this channel, and through the one predicate delivery's claim calls too.
            # The bound limits how often one task is woken, so two queues feeding one task
            # must not each get their own budget - which does mean a report can wait behind
            # parent-child traffic to a task that is both a parent and a supervisor. That is
            # the intended trade, said out loud. _rate_limited reads the same predicate before
            # the host is read; two callers both pass that, so this is where it actually holds.
            if reserve_send(db, self.policy, recipient, now) is not None:
                raise _Paced()
        return attempt_no, request_id, message

    def _settle(self, message_id, attempt_no, owner, request_id, facts, record, now) -> None:
        """Record what the transport answered, and where that leaves the message.

        An uncertain outcome is never retried here. There is no reconciler for this queue, and
        a second send that lands is a second wake for one fact - worse than a report that waits
        for somebody to look. It stays held_uncertain, which is not claimable, and says so.

        The message leaves sending only through the claim that holds it: this message, this
        attempt number and this lease owner, all three in the predicate of the write that
        moves it. A send slow enough for its lease to expire can be recovered by somebody else
        first, and recovery declares that attempt's outcome unknown. Matching held_uncertain
        as well as sending let the late receipt undo that declaration - a retry-safe refusal
        arriving after the recovery made the message claimable again, and a second attempt
        could wake the recipient while the first one's outcome was still unknown. So once an
        attempt is declared uncertain its late receipt moves nothing: it is recorded on its
        attempt, where the history keeps it, and only a verified readback moves the message.

        Expiry alone ends nothing. A claim whose lease ran out and that nobody has recovered
        still holds the row, and its receipt is still the best fact there is about its send.
        """
        attempts = attempt_no
        state = facts.delivery_state
        when, hold = None, None
        if state == DEFERRED_BUSY:
            when = now + self.policy.delay_for(attempts, "busy")
            if attempts >= self.policy.busy_max_attempts:
                hold = self.policy.cap_reason("busy")
        elif state == WITHHELD_PRE_SEND:
            when = now + self.policy.delay_for(attempts, "send")
            if attempts >= self.policy.max_attempts:
                hold = self.policy.cap_reason("send")
        at = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "UPDATE supervisor_attempts SET state = ?, send_attempted = ?, retry_safe = ?,"
                " turn_id = ?, record = ?, observed_at = ? WHERE request_id = ?",
                (state, facts.send_attempted, int(bool(facts.retry_safe)), facts.turn_id,
                 json.dumps(record, ensure_ascii=False, sort_keys=True), at, request_id),
            )
            # The attempt's own receipt is always recorded; the MESSAGE moves only while this
            # claim still holds it.
            cursor = db.execute(
                "UPDATE supervisor_messages SET state = ?, next_eligible_at = ?,"
                " hold_reason = ?, lease_owner = NULL, lease_until = NULL, updated_at = ?"
                " WHERE message_id = ? AND state = ? AND attempt_count = ?"
                "   AND lease_owner IS ?",
                (state, when, hold, at, message_id, SENDING, attempt_no, owner),
            )
            moved = cursor.rowcount == 1
            stands = db.execute(
                "SELECT state FROM supervisor_messages WHERE message_id = ?",
                (message_id,)).fetchone()
            self.store.journal(
                "supervisor_message_attempted", message_id,
                {"requestId": request_id, "attemptNo": attempt_no, "deliveryState": state,
                 "sendAttempted": facts.send_attempted, "turnId": facts.turn_id,
                 "holdReason": hold if moved else None, "messageMoved": moved,
                 "messageState": stands["state"] if stands is not None else None,
                 "reason": None if moved else
                 "this claim no longer held the message when its receipt arrived, so the"
                 " receipt is recorded on its attempt and the message is left where it is"},
                at=at)

    def _rate_limited(self, recipient, now):
        """A preflight over the claim's own predicate, which saves the host a read."""
        return send_refusal(self.store.db, self.policy, recipient, now) is not None

    def _reschedule(self, row, when, *, state=None, hold=None) -> None:
        with self.store.transaction() as db:
            self._reschedule_in(db, row, when, state=state, hold=hold)

    def _reschedule_in(self, db, row, when, *, state=None, hold=None) -> bool:
        # A compare-and-set on everything the caller's decision rested on: the state, the hold,
        # the recheck time, the recipient and the attempt count it observed. Matching only the
        # state class and the count let a stale caller clear another caller's busy cap, and
        # put the former recipient's delay back on a message re-addressed since it looked.
        cursor = db.execute(
            "UPDATE supervisor_messages SET state = ?, next_eligible_at = ?,"
            " hold_reason = ?, updated_at = ? WHERE message_id = ? AND state IN (?,?,?)"
            "   AND state = ? AND hold_reason IS ? AND next_eligible_at IS ?"
            "   AND recipient_task_id = ? AND attempt_count = ?",
            (state or row["state"], when, hold, self.clock.iso(), row["message_id"],
             QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND, row["state"], row["hold_reason"],
             row["next_eligible_at"], row["recipient_task_id"], row["attempt_count"]))
        return cursor.rowcount == 1

    def _defer_busy(self, row, now) -> None:
        """A recipient mid-turn is left strictly alone: no resume, no attempt, no record.

        There is nothing to classify, because no transport call was made.

        The deferral is counted in the journal rather than on the row, because attempt_count
        only moves inside the claim and a busy recipient never reaches one. Counting there
        made the busy cap unreachable AND froze the backoff at its base interval, so an
        endlessly busy supervisor was retried at a fixed rate for ever. The journal is durable
        and already exists, which is what makes this a bound rather than a hope.

        Counted, decided and written in ONE transaction. Counting before it let two callers
        deferring one message both read the same count, both journal, and reach the cap
        twice as fast; and a deferral whose reschedule found the row already moved was
        counted anyway.
        """
        with self.store.transaction() as db:
            deferrals = self._deferrals(row["message_id"]) + 1
            hold = (self.policy.cap_reason("busy")
                    if deferrals >= self.policy.busy_max_attempts else None)
            if self._reschedule_in(db, row, now + self.policy.delay_for(deferrals, "busy"),
                                   state=DEFERRED_BUSY, hold=hold):
                self.store.journal("supervisor_message_deferred", row["message_id"],
                                   {"deferral": deferrals, "holdReason": hold},
                                   at=self.clock.iso())

    def _deferrals(self, message_id) -> int:
        """How many times this message has been put off for its CURRENT recipient being busy.

        Counted from the latest re-address, so a successor does not inherit a cap its
        predecessor ran up.
        """
        row = self.store.one(
            "SELECT COUNT(*) AS seen FROM journal WHERE kind = ? AND subject = ?"
            "   AND seq > (SELECT COALESCE(MAX(seq), 0) FROM journal"
            "               WHERE kind = ? AND subject = ?)",
            ("supervisor_message_deferred", message_id, READDRESSED, message_id))
        return row["seen"] if row is not None else 0

    def _withhold(self, row, observation, now) -> None:
        """A recipient the host says cannot receive holds the report where it is.

        No hold_reason, deliberately. An archived, paused or usage-limited supervisor is a
        state somebody can undo, and a held row is skipped by every later attempt - so writing
        the lifecycle answer as a hold meant unarchiving never released the report and the one
        the user fixed stayed stuck. What the row gets is a recheck time; what the journal gets
        is why. A hold is for a bound this channel chose, not for a fact about the recipient
        that the next observation may contradict.
        """
        with self.store.transaction() as db:
            if self._reschedule_in(db, row, now + self.policy.lifecycle_recheck_seconds,
                                   state=WITHHELD_PRE_SEND):
                self.store.journal(
                    "supervisor_message_withheld", row["message_id"],
                    {"deliverable": observation.deliverable,
                     "reason": observation.withhold_reason}, at=self.clock.iso())

    def _withhold_settings(self, row, now, refusal) -> None:
        """Withheld before any transport call, naming what the record got wrong.

        Not a permanent hold: settings that were never recorded can be recorded, and the next
        pass decides again. Nothing was claimed, so there is no attempt to explain.
        """
        with self.store.transaction() as db:
            if self._reschedule_in(db, row, now + self.policy.lifecycle_recheck_seconds,
                                   state=WITHHELD_PRE_SEND):
                self.store.journal(
                    "supervisor_message_withheld", row["message_id"],
                    {"reason": refusal.reason.value if refusal.reason else None,
                     "detail": refusal.detail}, at=self.clock.iso())

    # ------------------------------------------------------------------ reading it back

    def read_back(self, message_id, *, read_turn_id, proof, adapter=None,
                  asserted_by=None) -> dict:
        """The recipient saying it read this, and what the host could establish about that.

        The proof is recomputed rather than trusted, and the turn is checked against the host's
        own list and against the moment the send started. A readback that does not verify is
        still recorded: hiding it would lose the fact that somebody answered, and the message
        stays where it was rather than moving to read.

        asserted_by is a DECLARATION and not an authentication. Every identifier the proof is
        computed from is in this store, and anyone who can call this can already write the row
        it produces, so nothing here can establish who is asking. What it can do is write down
        who claimed to be asking and refuse a claim that does not name this message's
        recipient, which turns an unstated assumption into a recorded fact.

        The verdict joins facts read at DIFFERENT moments. The turn is read first and the
        transcript scanned after it, each through its own paged host calls, and the host offers
        no snapshot or revision that could tie the two to one observation. What makes the join
        sound is that both facts are about append-only history: a turn keeps its id and start
        time once it has one, and an item keeps its text and its turn. A host that rewrites
        history between the two reads - a rollback removing the turn or the item - is not
        visible from here, and the verdict does not claim otherwise.

        A message held uncertain is read back too. Its send's response was lost, or its sender
        died between the claim and the receipt, and nothing else reconciles this queue - so
        refusing it here made the one check that could prove this exact attempt arrived
        unreachable. It is verified against THAT attempt's request id, and only a verified
        readback settles it: the answer alone never does, and an unverified one is recorded
        with the message left where it was.
        """
        row = self._recover_if_stranded(self.get(message_id), self.clock.now())
        if asserted_by is not None and asserted_by != row["recipient_task_id"]:
            raise DeliveryRefused(
                RefusalReason.RECIPIENT_NOT_AUTHORIZED,
                repr(asserted_by) + " is not the recipient of this message, which is "
                + repr(row["recipient_task_id"]) + ". The declaration is checked against the"
                " row; it is not evidence of who is calling, and nothing on this side could"
                " be",
            )
        if row["state"] not in DELIVERED + (HELD_UNCERTAIN, READ):
            raise DeliveryRefused(
                RefusalReason.NOT_CLAIMABLE,
                "message " + repr(message_id) + " is " + repr(row["state"]) + "; only a"
                " message that was sent, or whose send nobody heard back from, is read back,"
                " because otherwise there is nothing yet to have read",
            )
        if not isinstance(read_turn_id, str) or not read_turn_id.strip():
            raise DeliveryRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a readback names the turn it was written in; without one there is nothing to"
                " check against the host",
            )
        existing = self._settled_readback(message_id)
        if existing is not None:
            # Settled. A second reading of one message is the same fact, and answering it with
            # what stands is what makes an uncertain submission safe to settle by asking.
            # This is a fast path and it returns BEFORE the proof is checked, so a later
            # proof-valid answer is not recorded and a later mismatch is not refused. That is
            # deliberate and it is why the row below says what it says.
            return self._answer(existing, recorded=False)
        expected = supervisor_read_proof(message_id, read_turn_id)
        if proof != expected:
            raise DeliveryRefused(
                RefusalReason.ACK_PROOF_MISMATCH,
                "the proof does not match this message and turn. It is sha256(messageId|your"
                " own turn id), and quoting the delivered fields back cannot produce it",
            )
        uncertain = row["state"] == HELD_UNCERTAIN
        if uncertain:
            # The attempt whose outcome nobody heard. Its own request id is the token, so what
            # the scan finds is evidence about THAT send and no other. Found by its number and
            # not by its state: a receipt arriving after the recovery is recorded on the
            # attempt without moving the message, so the attempt can say dispatched, or that
            # nothing was sent, while the message is still held - and the transcript is still
            # the fact that settles it.
            attempt = self.store.one(
                "SELECT * FROM supervisor_attempts WHERE message_id = ? AND attempt_no = ?",
                (message_id, row["attempt_count"]))
            if attempt is None:
                raise DeliveryRefused(
                    RefusalReason.NOT_CLAIMABLE,
                    "message " + repr(message_id) + " is held uncertain with no attempt "
                    + str(row["attempt_count"]) + " to read it back against, so there is no"
                    " token to look for")
        else:
            attempt = self.store.one(
                "SELECT * FROM supervisor_attempts WHERE message_id = ? AND state IN (?,?)"
                " ORDER BY attempt_no DESC LIMIT 1", (message_id, DISPATCHED, INBOX_ONLY))
        verified, detail, origin, read_turn = self._verify_read_turn(
            row, attempt, read_turn_id, adapter)
        delivered = self._delivered_evidence(row, attempt, adapter)
        if verified == HOST_READ and not delivered.get("found"):
            # The turn is real; the message is not established as having reached the place the
            # recipient reads. Saying read on the strength of the turn alone would rest the
            # whole claim on our own send receipt, which is the one thing a roundtrip is
            # supposed to be independent of - and the scan answers three ways, so an
            # unreadable or truncated one is not absence either.
            verified = TRANSCRIPT_UNCONFIRMED
            detail = ("the turn is real and the transcript does not confirm the message: "
                      + str(delivered.get("reason")
                            or ("scanned " + str(delivered.get("itemsRead"))
                                + " items and did not find " + str(delivered.get("token"))
                                + ("" if delivered.get("exhausted")
                                   else ", and the scan was not exhausted, so this is"
                                   " inconclusive rather than absence"))))
        elif verified == HOST_READ and not delivered.get("turnId"):
            # Found, and in no turn the host would name. Every other check ties the evidence
            # to a turn - the named one, or one the named turn follows - and a token in an
            # unknown place is tied to neither, so it is not a verification of anything.
            verified = TRANSCRIPT_UNCONFIRMED
            detail = ("the transcript holds this attempt's request id and the host did not say"
                      " which turn it is in, so it is not tied to the named turn or to any turn"
                      " that turn follows")
        elif (verified == HOST_READ and origin == RELAY_OPENED
              and delivered.get("turnId")
              and delivered["turnId"] != read_turn_id):
            # The readback claims the turn this send named, and the delivered token is in a
            # different one. Where the message landed is where a reader of it reads, so the
            # two disagreeing is an inconsistent claim rather than a verified read. It ties
            # the named turn to the delivered evidence in the one case where they CAN be
            # tied; a turn the recipient opened afterwards is not expected to hold the token,
            # and is the stronger case anyway because the sender did not know its id.
            verified = TRANSCRIPT_TURN_MISMATCH
            detail = ("this readback names the turn the send opened, " + str(read_turn_id)
                      + ", and the delivered token is in " + str(delivered["turnId"])
                      + "; where the message landed is where a reader of it reads")
        elif (verified == HOST_READ and delivered.get("turnId")
              and delivered["turnId"] != read_turn_id):
            # The named turn has to follow the turn the message LANDED in, which is a host fact
            # and closes what a send time cannot: the transport can queue a message after it
            # is called, so a turn opened in that interval followed the stamp and still came
            # before the bytes. An unknown start is not established, and is not verified.
            earlier = _began_before(read_turn.started_at if read_turn else None,
                                    self._turn_start(row, delivered["turnId"], adapter))
            if earlier is None:
                verified = NO_HOST
                detail = ("the turn this message landed in, " + str(delivered["turnId"])
                          + ", has no start time the host would give, so whether "
                          + str(read_turn_id) + " followed it is not established")
            elif earlier:
                verified = TURN_PREDATES_SEND
                detail = (str(read_turn_id) + " began before " + str(delivered["turnId"])
                          + ", the turn this message landed in, so it cannot be the turn"
                          " that read it")
        reconciled = None
        if uncertain and verified == HOST_READ:
            reconciled = {"from": HELD_UNCERTAIN, "by": "readback",
                          "requestId": attempt["request_id"],
                          "attemptNo": attempt["attempt_no"],
                          "deliveredTurnId": delivered.get("turnId")}
            detail += ("; the send's own response was never heard, and this attempt's request"
                       " id in the recipient's transcript is what settles it")
        at = self.clock.iso()
        with self.store.transaction() as db:
            # Re-read FIRST, inside the write, because the check above happens outside the
            # lock and can be raced: two readbacks of one message both pass it, and whichever
            # commits second replaces the first - so an unverified answer overwrites a
            # verified one while the message row stays read, which is the two records
            # disagreeing about the same fact. A verified readback is settled, whatever the
            # second caller brought. BEGIN IMMEDIATE serialises the writers, so the loser of
            # the race reads the winner's row here rather than its own stale absence.
            settled = db.execute(
                "SELECT * FROM supervisor_readbacks WHERE message_id = ? AND verified = ?",
                (message_id, HOST_READ),
            ).fetchone()
            if settled is not None:
                return self._answer(settled, recorded=False, raced=True)
            # And the message is still the one these checks were made against. They ran outside
            # the lock, so a move in between would have them describe a state that is gone.
            current = db.execute(
                "SELECT state, attempt_count FROM supervisor_messages WHERE message_id = ?",
                (message_id,)).fetchone()
            if (current["state"], current["attempt_count"]) != (row["state"],
                                                                 row["attempt_count"]):
                raise DeliveryRefused(
                    RefusalReason.NOT_CLAIMABLE,
                    "message " + repr(message_id) + " moved from " + repr(row["state"])
                    + " to " + repr(current["state"]) + " while this readback was being"
                    " checked, so the checks describe a message that is no longer there."
                    " Nothing was recorded; answer again")
            db.execute(
                "INSERT INTO supervisor_readbacks (message_id, read_turn_id, proof, verified,"
                " request_id, detail, read_at) VALUES (?,?,?,?,?,?,?)"
                " ON CONFLICT(message_id) DO UPDATE SET read_turn_id = excluded.read_turn_id,"
                " proof = excluded.proof, verified = excluded.verified,"
                " request_id = excluded.request_id, detail = excluded.detail,"
                " read_at = excluded.read_at",
                (message_id, read_turn_id, proof, verified,
                 attempt["request_id"] if attempt is not None else None,
                 json.dumps({"turnOrigin": origin, "detail": detail,
                             "delivered": delivered,
                             "assertedBy": asserted_by or "undeclared",
                             "reconciled": reconciled},
                            ensure_ascii=False, sort_keys=True),
                 at),
            )
            if verified == HOST_READ:
                db.execute(
                    "UPDATE supervisor_messages SET state = ?, updated_at = ?"
                    " WHERE message_id = ? AND state = ? AND attempt_count = ?",
                    (READ, at, message_id, row["state"], row["attempt_count"]))
            if reconciled is not None:
                self.store.journal("supervisor_message_reconciled", message_id,
                                   {**reconciled, "readTurnId": read_turn_id}, at=at)
            self.store.journal(
                "supervisor_message_read", message_id,
                {"verified": verified, "turnOrigin": origin, "readTurnId": read_turn_id,
                 "deliveredEvidence": delivered.get("found"),
                 "reconciled": reconciled is not None}, at=at)
            written = db.execute(
                "SELECT * FROM supervisor_readbacks WHERE message_id = ?", (message_id,)
            ).fetchone()
        return self._answer(written, recorded=True)

    def _answer(self, readback, *, recorded, raced=False) -> dict:
        """One readback answer, built from the stored row, whichever path reached it.

        The first answer, a later one answered from the settled row and one that lost the race
        to settle it are the same fact, so they have the same shape: every field decoded from
        what was stored, with recorded and raced saying which of the three this was. The
        second used to hand back the stored detail as JSON text and drop the turn's origin,
        the transcript evidence, who asserted it and how it was reconciled.
        """
        stored = json.loads(readback["detail"]) if readback["detail"] else {}
        return {"schema": VERSION, "messageId": readback["message_id"],
                "recorded": recorded, "raced": raced,
                "verified": readback["verified"], "readTurnId": readback["read_turn_id"],
                "turnOrigin": stored.get("turnOrigin"), "detail": stored.get("detail"),
                "delivered": stored.get("delivered"), "readAt": readback["read_at"],
                "assertedBy": stored.get("assertedBy"), "reconciled": stored.get("reconciled"),
                "establishes": _establishes(readback["verified"], stored.get("turnOrigin")),
                "limits": READBACK_LIMITS}


    def _settled_readback(self, message_id):
        """The verified readback this message already has, or None.

        Its own method so a case can make it answer nothing, which is exactly what a caller
        that lost a race sees: it looked, found nothing, and by the time it writes somebody
        else has settled the message. A sequential test cannot reach that state, and the guard
        inside the write is what actually holds - this read is only an optimisation that saves
        the host work when the answer is already settled.
        """
        return self.store.one(
            "SELECT * FROM supervisor_readbacks WHERE message_id = ? AND verified = ?",
            (message_id, HOST_READ))

    def _verify_read_turn(self, row, attempt, read_turn_id, adapter):
        """A readback has to come from a real turn that did not start before the send.

        The turn the send itself opened is allowed, and is the ordinary case: the message is
        what wakes the supervisor, so that turn is where it lands. Which turn was named is
        returned beside the verdict rather than folded into it, because the sender already
        knows the id of the turn it opened, so for that turn the verdict shows arrival and not
        reading, and a reader deserves to know which it is.
        """
        recipient = row["recipient_task_id"]
        origin = ORIGIN_UNKNOWN
        if attempt is not None and attempt["turn_id"]:
            origin = RELAY_OPENED if attempt["turn_id"] == read_turn_id else RECIPIENT_OPENED
        if adapter is None:
            return NO_HOST, ("no host adapter in this process, so whether this turn is real was"
                             " not established"), origin, None
        # read_turn is asked directly rather than checking a listing first. The listing is
        # bounded - the last 25 turns - so a recipient that has taken a few turns since being
        # woken pushes a perfectly real read turn off the end of it, and the readback came
        # back turn_not_found for a turn the host would have handed over on request. The read
        # is scoped to this thread, so it establishes existence AND membership in one answer,
        # and a bound cannot make a real turn disappear.
        try:
            turn = adapter.read_turn(recipient, read_turn_id)
        except Exception as error:  # noqa: BLE001
            return NO_HOST, "the turn could not be read: " + str(error), origin, None
        if turn is None:
            return TURN_NOT_FOUND, "the host has no such turn on this thread", origin, None
        began = _host_time(turn.started_at)
        if began is None:
            return NO_HOST, ("the host gave no start time for this turn that is a time, and an"
                             " unknown chronology is not a verification"), origin, turn
        # Applied to EVERY candidate, including the turn the send reports having opened. That
        # turn is not always a new one: the transport can steer an existing turn, and the
        # delivery path keeps a whole flag for that case, so exempting it let a turn that
        # predates the message verify a readback for the message. The precision allowance
        # covers a turn genuinely started by this send.
        # Measured from the instant the transport started and from nothing earlier: the claim
        # time let a turn opened between the claim and the call pass as if it followed the
        # send. An attempt with no transport instant has no send to measure against, and a
        # readback that cannot be tied to the send does not verify.
        started = _iso_time(attempt["transport_started_at"]) if attempt is not None else None
        if started is None:
            return NO_HOST, ("this attempt has no recorded transport start, so there is no send"
                             " to measure the turn against"), origin, turn
        if began + TURN_START_PRECISION_SECONDS <= started:
            return TURN_PREDATES_SEND, ("this turn began before the send, so it cannot be the"
                                        " turn that read it"), origin, turn
        return HOST_READ, ("the host lists this turn on the recipient's thread and it did not"
                           " begin before the send"), origin, turn

    def _turn_start(self, row, turn_id, adapter):
        """When a turn on the recipient's thread began, or None when the host will not say."""
        if adapter is None:
            return None
        try:
            turn = adapter.read_turn(row["recipient_task_id"], turn_id)
        except Exception:  # noqa: BLE001 - an unreadable turn is an unestablished start
            return None
        return turn.started_at if turn is not None else None

    def _delivered_evidence(self, row, attempt, adapter) -> dict:
        """Whether the bytes this send froze are in the recipient's own transcript.

        Independent of our own receipt on purpose. A receipt says the transport accepted the
        call; a token found in the recipient's items says the message is where the recipient
        reads. The request id is the token because it is unique to one attempt and is inside
        the bytes that attempt sent.

        A truncated scan is inconclusive and is reported as such, never as absence.
        """
        if adapter is None:
            return {"scanned": False,
                    "reason": "no host adapter in this process, so the transcript was not read"}
        if attempt is None:
            return {"scanned": False,
                    "reason": "no dispatched attempt, so there is no token to look for"}
        try:
            scan = adapter.find_token(
                row["recipient_task_id"], attempt["request_id"], limit=TRANSCRIPT_SCAN)
        except Exception as error:  # noqa: BLE001
            return {"scanned": False,
                    "reason": "the transcript could not be read: " + str(error)}
        return {"scanned": True, "found": bool(scan.found), "turnId": scan.turn_id,
                "exhausted": bool(scan.exhausted), "itemsRead": scan.scanned,
                "token": attempt["request_id"]}

    # ------------------------------------------------------------------- how far it got

    def reach(self, message_id) -> dict:
        """The five stages, answered from these two tables and from nothing else.

        agreed, applied and verified stay not_applicable whatever happens here. A supervisor
        agreeing, acting or confirming is not a fact this store holds, and the envelope's own
        table refuses a caller that tries to write one in.
        """
        ladder = envelope.unreached(envelope.PARENT_TO_SUPERVISOR)
        row = self.find(message_id)
        if row is None:
            return ladder
        attempt = self.store.one(
            "SELECT * FROM supervisor_attempts WHERE message_id = ? ORDER BY attempt_no DESC"
            " LIMIT 1", (message_id,))
        if attempt is not None:
            if attempt["state"] in DELIVERED:
                ladder[envelope.TRANSPORT_ACCEPTED] = envelope.stage(
                    envelope.YES, source="supervisor_attempts",
                    detail="the transport accepted " + attempt["request_id"])
            elif attempt["state"] in (WITHHELD_PRE_SEND, DEFERRED_BUSY):
                ladder[envelope.TRANSPORT_ACCEPTED] = envelope.stage(
                    envelope.NO, source="supervisor_attempts",
                    detail="nothing was sent: " + attempt["state"])
            # held_uncertain stays unmeasured, which is the whole point of that state: the
            # transport did not answer, and an unanswered send is not a refused one.
        readback = self.store.one(
            "SELECT * FROM supervisor_readbacks WHERE message_id = ?", (message_id,))
        if readback is not None:
            if readback["verified"] == HOST_READ:
                origin = (json.loads(readback["detail"]) if readback["detail"]
                          else {}).get("turnOrigin")
                ladder[envelope.RECEIVED] = envelope.stage(
                    envelope.YES, source="supervisor_readbacks",
                    detail="read back from turn " + readback["read_turn_id"] + " ("
                           + str(origin) + "): " + _establishes(HOST_READ, origin))
            else:
                ladder[envelope.RECEIVED] = envelope.stage(
                    envelope.UNMEASURED,
                    detail="a readback was recorded and did not verify: "
                           + readback["verified"])
        envelope.check_reach(envelope.PARENT_TO_SUPERVISOR, ladder)
        return ladder

    def show(self, message_id) -> dict:
        """One staged message whole: what it says, every attempt, and what came back."""
        row = self.get(message_id)
        attempts = [
            {"requestId": one["request_id"], "attemptNo": one["attempt_no"],
             "state": one["state"], "sendAttempted": one["send_attempted"],
             "retrySafe": bool(one["retry_safe"]), "turnId": one["turn_id"],
             "sentAt": one["sent_at"], "transportStartedAt": one["transport_started_at"],
             "observedAt": one["observed_at"],
             "message": one["message"]}
            for one in self.store.all(
                "SELECT * FROM supervisor_attempts WHERE message_id = ? ORDER BY attempt_no",
                (message_id,))
        ]
        readback = self.store.one(
            "SELECT * FROM supervisor_readbacks WHERE message_id = ?", (message_id,))
        stored = (json.loads(readback["detail"]) if readback is not None and readback["detail"]
                  else {})
        origin = stored.get("turnOrigin") if readback is not None else None
        frozen = json.loads(row["reading"]) if row["reading"] else None
        return {
            "schema": VERSION,
            "messageId": message_id,
            "obligationId": row["obligation_id"],
            "kind": row["obligation_kind"],
            "relationshipId": row["relationship_id"],
            "projectKey": row["project_key"],
            "purpose": row["purpose"],
            "sender": row["sender_task_id"],
            "recipient": row["recipient_task_id"],
            "state": row["state"],
            # Beside the state, because "read" is shorter than what it proves.
            "turnOrigin": origin if row["state"] == READ else None,
            "readEstablishes": (_establishes(readback["verified"], origin)
                                if row["state"] == READ and readback is not None else None),
            "holdReason": row["hold_reason"],
            "stagedAt": row["staged_at"],
            "packet": json.loads(row["packet"]),
            # An omission's evidence: the reading it was staged from, frozen, and the command
            # that re-reads the turn now. The two can disagree, and the packet is about the first.
            "stagedFrom": None if frozen is None else {
                "reading": frozen,
                "recheck": _recheck_line(frozen),
                "note": "the reading this report was composed from, frozen when it was staged."
                        " recheck re-reads the turn now and can answer differently - a report"
                        " that reached the turn afterwards is news about the turn, and does not"
                        " change what this message said"},
            "attempts": attempts,
            "readback": None if readback is None else {
                "readTurnId": readback["read_turn_id"], "verified": readback["verified"],
                "turnOrigin": stored.get("turnOrigin"),
                "establishes": _establishes(readback["verified"], stored.get("turnOrigin")),
                "requestId": readback["request_id"], "readAt": readback["read_at"],
                "detail": stored or None},
            "reach": self.reach(message_id),
            "limits": "this is what the relay staged, sent and was told. Whether the supervisor"
                      " acted on it is not here, and what discharges the obligation is still"
                      " the Linear record it reads for itself, confirmed",
        }




def _selectors(reading):
    """The five selectors reporting-show needs, plus the state directory, or None.

    All or nothing on purpose. A partial set renders a command that looks runnable and is
    not, and the caller learns that only when they try it - so a reading missing any of them
    is treated as no reading at all, and staging says so rather than shipping the line.
    """
    if not isinstance(reading, dict):
        return None
    found = reading.get("selectors")
    if not isinstance(found, dict):
        return None
    wanted = ("state", "markerRoot", "workspace", "assignment", "session", "turn")
    if any(not str(found.get(name) or "").strip() for name in wanted):
        return None
    return {name: found[name] for name in wanted}

def _artifact(report):
    """The pull request a work report names, when it names one whole.

    A number without its repository belongs to whoever reads it first, and a pull request
    without its head is inherited by the next push to it, so a partial one is left out rather
    than reported in pieces. Nothing requires this field on a report upward, which is exactly
    why leaving it out is available: a completion with no repository change has none.
    """
    if not report:
        return None
    repository = report.get("repository")
    number = report.get("prNumber")
    head = report.get("headSha")
    if not (repository and number and head):
        return None
    return packets.pull_request(repository=repository, number=number, head_sha=head)
