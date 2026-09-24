"""The deliverer that carries fault notifications to the level above (CRW-205 criterion 7).

The fault ledger decides what is owed upward and whether it may go now: its eligibility (the
user's pause and archive of the assignment, and whether its parent can be contacted) and its
product's budget, both at reservation. The supervisor channel decides how a report reaches the
level above: who that is (the live linkage), whether the recipient may be woken, the re-check
where the transport starts (I-247) and what a lost answer means. This module is the seam between
the two and owns no rule of its own. Each tick it:

- settles uncertain notifications from what the channel recorded for their one message - sent
  (dispatched, or read back) is delivered; a message that provably sent nothing returns the
  notification to pending; a send that may have happened is left uncertain, and never repeated;
- asks, without writing, whether any pending notification can go now, measuring a parent whose
  contact the ledger's eligibility could not read (the host observation delivery itself records,
  so the rule has its input) and recording why a notification waits only when that changes;
- reserves (reserve_notifications, whose predicate this module supplies: it answers only from the
  channel's own records, before any budget is spent), stages each reserved notification as one
  supervisor message, attempts it, and settles it from the channel's recorded answer.

A notification is never sent sideways: when nothing places it under a project whose supervisor
the linkage names, it waits with that reason.
"""

from datetime import datetime, timezone

from . import faults, intent, lifecycle, supervision
from .errors import DeliveryRefused
from .supervisorchannel import CLAIMABLE, PARKED_HOLD, READ, UNADDRESSED_HOLD, _addressed_as
from .transport import DISPATCHED, SENDING

# How many pending or uncertain notifications one tick reads, a page at a time, resuming after the
# last one the previous tick read. The position is kept in memory, as the daemon's supervisor
# pass keeps its own: losing it on a restart only changes where the next rotation starts.
PAGE = 20
SENT = (DISPATCHED, READ)
# The journal kinds in which the channel says why a message was not sent on an attempt.
NOT_SENT_BECAUSE = ("supervisor_message_withheld", "supervisor_message_deferred",
                    "supervisor_message_paced", "supervisor_message_superseded")


class NoticeDeliverer:
    def __init__(self, ledger, channel, *, owner):
        self.ledger = ledger
        self.channel = channel
        self.store = channel.store
        self.clock = channel.clock
        self.owner = owner
        self._pending_after = None
        self._uncertain_after = None

    def tick(self, adapter, *, now=None, limit) -> dict:
        """One bounded pass. Returns what it delivered, returned to pending, measured (host
        observations of a parent, each written as delivery writes one) and saw waiting."""
        now = self.clock.now() if now is None else now
        answer = {"delivered": 0, "returned": 0, "measured": 0, "waiting": []}
        self._reconcile(answer, now)
        if limit > 0 and self._ready(adapter, answer, now):
            taken = self.ledger.reserve_notifications(
                owner=self.owner, limit=limit,
                deliverable=lambda one: self.waiting_for(one, now=now))
            for one in taken["reserved"]:
                self._deliver(one, adapter, answer, now)
        return answer

    # ------------------------------------------------------------------ what can go now

    def waiting_for(self, notification, *, now=None):
        """Why this notification cannot be carried now, from the channel's own records, or None.

        Asked inside reserve_notifications, before any budget is spent, and by the pre-pass. It
        reads what the channel already decided about this notification's one message - in
        flight, held for a bound the channel chose, or rescheduled for a recheck - and asks
        resolve() who the level above is, and whether an earlier report to it can go first (the
        channel's own order). It never observes a host, never opens a transaction - it runs
        inside reserve_notifications' write - and adds no rule. Any failure is a reason to wait.
        """
        now = self.clock.now() if now is None else now
        try:
            return self._waiting_for(notification, now)
        except Exception as error:  # noqa: BLE001 - a question it cannot answer is a wait
            return "whether it can be carried now could not be read: " + _said(error)

    def _waiting_for(self, notification, now):
        # What the notification is about NOW - its relationship, else its scope's project
        # (faults.notice_facts, the reading stage_notice addresses from) - read on the store's
        # own connection, never a transaction of its own: this is asked inside
        # reserve_notifications' write, where a second BEGIN is refused. A staged message
        # addressed from another anchor is re-addressed by the next staging when nothing of it
        # was sent.
        notice = faults.notice_facts(self.store.db, notification["notificationId"])
        relation = notice["anchor"] if notice is not None else None
        row = self.channel.notice_message(notification["notificationId"])
        if row is not None:
            if row["state"] in SENT:
                return None
            if row["state"] not in CLAIMABLE:
                return ("its message " + row["message_id"] + " is " + row["state"]
                        + ": a send may be under way, and it is settled from that send's answer")
        if not relation:
            return ("no relationship this store holds places fault " + notification["faultId"]
                    + " under a project, and its scope names no project, so there is no level"
                    " above to tell")
        unfit = faults.unfit_notice(notice)
        if unfit is not None:
            return ("the fault's " + unfit + " is not a value the ledger writes, so no notice"
                    " can carry it; fault-show has the fault")
        try:
            resolution = self.channel.resolve(relation)
        except DeliveryRefused as refusal:
            return _said(refusal)
        unfit = faults.unfit_hierarchy(resolution)
        if unfit is not None:
            return ("the " + unfit + " the linkage names is not a plain identifier, so no notice"
                    " can carry it; fault-show has the fault")
        if row is not None and _addressed_as(row, resolution):
            # The bounds the channel put on the message - a busy or attempt cap, a lifecycle
            # recheck, a backoff - are about the task it is addressed to, so they hold only
            # while that is still the level above. Addressed to a former sender or supervisor,
            # they went with the handover: the next staging re-addresses the message and
            # releases them, exactly as the channel re-addresses a report.
            if row["hold_reason"] not in (None, PARKED_HOLD, UNADDRESSED_HOLD):
                return ("its message " + row["message_id"] + " is held by the supervisor"
                        " channel: " + row["hold_reason"])
            if row["next_eligible_at"] is not None and row["next_eligible_at"] > now:
                return ("the supervisor channel rechecks the level above at "
                        + _at(row["next_eligible_at"]) + self._because(row["message_id"]))
        # Ahead of its row in the recipient's queue, or - addressed to another recipient until
        # its next staging, or not staged yet - of any message to the recipient of now.
        same = row is not None and row["recipient_task_id"] == resolution["recipient"]
        ahead = self.channel.queued_ahead(resolution["recipient"], now=now,
                                          row=row if same else None)
        if ahead is not None:
            return ("an earlier report to " + resolution["recipient"] + " goes first: message "
                    + ahead)
        return None

    def _ready(self, adapter, answer, now):
        """Whether any pending notification on this page may be reserved now. Writes only a
        waiting reason that changed, and a host observation of a parent nobody has measured."""
        page = self.ledger.notifications(state=faults.PENDING, limit=PAGE,
                                         after=self._pending_after)
        self._pending_after = page["next"]
        ready, measured, unobserved = False, set(), {}
        for one in page["notifications"]:
            eligibility = one.get("eligibility") or {}
            if not eligibility.get("eligible"):
                parent = eligibility.get("parentTaskId")
                if (parent and parent not in measured and parent not in unobserved
                        and self._unmeasured(eligibility.get("contact"), now)):
                    try:
                        self._measure(adapter, parent)
                    except Exception as error:  # noqa: BLE001 - one parent, not the pass
                        # A host that cannot answer about one parent is that parent's
                        # notifications' reason to wait, not every other notification's: the
                        # pass goes on, and the next tick asks the host again.
                        unobserved[parent] = ("its parent " + parent + " could not be observed"
                                              " (" + type(error).__name__ + "), so whether it"
                                              " can be contacted is unknown")
                    else:
                        measured.add(parent)
                        answer["measured"] += 1
                        ready = True
                if parent in unobserved:
                    self._wait(one, unobserved[parent], answer)
                continue
            reason = self.waiting_for(one, now=now)
            if reason is None:
                ready = True
            else:
                self._wait(one, reason, answer)
        return ready

    def _wait(self, one, reason, answer):
        """Keep why a notification waits on it, written only when that changes."""
        if self.ledger.notification_waiting(one["notificationId"], reason=reason)["changed"]:
            answer["waiting"].append((one["notificationId"], reason))

    @staticmethod
    def _unmeasured(contact, now):
        """Whether the ledger's eligibility read no current observation of the parent: none,
        one without a time, one older than the window it treats as current, or one dated in the
        future beyond the tolerance supervision.contactable allows (a clock that went back) - a
        stored 'cannot be contacted' included, which is otherwise never read again."""
        if not contact:
            return False
        if contact.get("contactable") is None:
            return contact.get("asked", True)
        if contact.get("contactable") is True:
            return False
        moment = intent.moment(contact.get("observedAt"))
        if moment is None:
            return True
        age = now - moment.timestamp()
        return age > supervision.CONTACT_FRESH_FOR or age < -supervision.CONTACT_FUTURE_TOLERANCE

    def _measure(self, adapter, task):
        """The host observation delivery records when it reaches a task, recorded the same way."""
        lifecycle.record(self.store, self.clock, lifecycle.observe(
            adapter, task, require_evidence=self.channel.require_lifecycle_evidence))

    # ------------------------------------------------------------------ carrying it

    def _deliver(self, one, adapter, answer, now):
        notification, token = one["notificationId"], one["token"]
        notice = faults.notice_facts(self.store.db, notification)
        try:
            staged = self.channel.stage_notice(notice)
        except Exception as error:  # noqa: BLE001 - nothing staged is nothing sent
            self._return(notification, token, None, "nothing was staged: " + _said(error),
                         answer)
            return
        message_id = staged["messageId"]
        error = None
        if staged["message"]["state"] in CLAIMABLE:
            try:
                self.channel.attempt(message_id, adapter, now=now, owner=self.owner)
            except Exception as failure:  # noqa: BLE001 - the row says what happened
                error = failure
        row = self.channel.get(message_id)
        if row["state"] in SENT:
            self._settled(self.ledger.ack_notification, notification, answer, "delivered",
                          token=token, ref=self._ref(row))
        elif row["state"] in CLAIMABLE and self.channel.may_have_sent(message_id) is None:
            self._return(notification, token, message_id, self._why(row, error), answer)
        # Otherwise a send may be under way or may have landed: the reservation stays, a lapse
        # makes the notification uncertain, and _reconcile settles it from the channel's answer.

    def _return(self, notification, token, message_id, why, answer):
        self._settled(self.ledger.fail_notification, notification, answer, "returned",
                      token=token, error=why)
        if message_id is not None:
            self.channel.park_notice(message_id, why)

    def _reconcile(self, answer, now):
        page = self.ledger.notifications(state=faults.UNCERTAIN, limit=PAGE,
                                         after=self._uncertain_after)
        self._uncertain_after = page["next"]
        for one in page["notifications"]:
            notification = one["notificationId"]
            if one.get("owner") != self.owner:
                # Reserved by another owner - the fault-notification-reserve command, another
                # transport - which may have sent it by its own means. The absence of a
                # message on this channel proves nothing about that send, so it stays uncertain
                # for its owner's transport or fault-notification-reconcile to settle (I-444).
                continue
            row = self.channel.notice_message(notification)
            if row is not None and row["state"] == SENDING:
                # A claim whose process died: settled exactly as attempt() would first - queued
                # again when its transport never started, held uncertain when it may have.
                row = self.channel.recover(row["message_id"], now=now)
            if row is None:
                self._settled(self.ledger.reconcile_notification, notification, answer,
                              "returned", delivered=False,
                              ref="no message was ever staged for it, so nothing was sent")
            elif row["state"] in SENT:
                self._settled(self.ledger.reconcile_notification, notification, answer,
                              "delivered", delivered=True, ref=self._ref(row))
            elif (row["state"] in CLAIMABLE
                  and self.channel.may_have_sent(row["message_id"]) is None):
                why = self._why(row, None)
                self._settled(self.ledger.reconcile_notification, notification, answer,
                              "returned", delivered=False, ref=why)
                self.channel.park_notice(row["message_id"], why)
            # held_uncertain or a live claim: left uncertain. Only the channel's recorded answer
            # - a verified readback - settles a send that may have landed.

    @staticmethod
    def _settled(settle, notification, answer, counted, **kw):
        try:
            settle(notification, **kw)
        except faults.FaultRefused:
            # The reservation lapsed or was settled meanwhile; the next pass reads it as it is.
            return
        answer[counted] += 1

    def _ref(self, row):
        attempt = self.store.one(
            "SELECT request_id FROM supervisor_attempts WHERE message_id = ?"
            " ORDER BY attempt_no DESC LIMIT 1", (row["message_id"],))
        return ("supervisor message " + row["message_id"] + " " + row["state"]
                + (", attempt " + attempt["request_id"] if attempt is not None else ""))

    def _why(self, row, error):
        parts = ["nothing was sent: its message " + row["message_id"] + " is " + row["state"]]
        if row["next_eligible_at"] is not None:
            parts.append("rechecked at " + _at(row["next_eligible_at"]))
        if error is not None:
            parts.append(_said(error))
        return ", ".join(parts) + self._because(row["message_id"])

    def _because(self, message_id):
        seen = self.store.one(
            "SELECT kind, detail FROM journal WHERE subject = ? AND kind IN (?,?,?,?)"
            " ORDER BY seq DESC LIMIT 1", (message_id, *NOT_SENT_BECAUSE))
        if seen is None:
            return ""
        detail = faults._json(seen["detail"])
        reason = detail.get("reason") or detail.get("detail") or detail.get("refusal")
        return " (" + seen["kind"] + (": " + str(reason) if reason else "") + ")"


def _at(seconds):
    """An epoch time the way this store writes its own instants."""
    return datetime.fromtimestamp(seconds, timezone.utc).isoformat(timespec="seconds")


def _said(error):
    reason = getattr(error, "reason", None)
    detail = getattr(error, "detail", None)
    if reason is not None and detail:
        return getattr(reason, "value", str(reason)) + ": " + str(detail)
    return type(error).__name__ + ": " + str(error)
