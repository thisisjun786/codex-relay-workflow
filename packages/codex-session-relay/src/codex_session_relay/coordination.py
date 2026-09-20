"""Primitives the coordination modules share: derived identity, refusals, contests.

Three modules sit on this - the merge turn, execution capacity, and shared edit regions - and
all three need the same two things the linkage tables already established: an identifier
derived from the fields that define a record, so replaying one operation converges rather than
opening a second; and a refusal that is RETAINED, because a contest that only raises is
invisible to every later reader.

Kept here rather than imported from linkage for two reasons. Linkage's equivalent is a private
name, and three modules leaning on another module's underscore is a coupling the next edit to
that module breaks silently. And linkage_conflicts is keyed and read by linkage's own
vocabulary of scope kinds; writing merge targets and edit regions into it would make
Linkage.conflicts answer about domains it knows nothing of.

The write protocol every caller follows, stated once here so no method has to re-argue it:
open store.transaction(), read and validate COMPLETELY, then either record the conflict alone
or write the whole operation. Never part of one. The refusal is raised after the transaction
closes, so the evidence survives the rollback that would otherwise take it.
"""

from .errors import CoordinationError, RefusalReason
from .identity import sha256_hex

DOMAIN_MERGE_TARGET = "merge_target"
DOMAIN_EXECUTION = "execution_subject"
DOMAIN_EDIT_REGION = "edit_region"

# 128 bits, matching the linkage identifiers and for the same reason: these are relay-owned so
# nothing freezes the width, and a collision here would silently MERGE two turns or two
# agreements rather than fail where somebody would see it.
ID_WIDTH = 32

SEPARATOR = "|"


def exact(value, what):
    """A field that enters a derivation: a non-empty string that cannot corrupt it.

    The separator check is not decoration. Without it 'a|b' and 'a' plus 'b' derive the same
    identifier, so a caller could reach another record's identity by spelling one field oddly.
    """
    if not isinstance(value, str) or not value.strip():
        raise CoordinationError(
            RefusalReason.UNREGISTERED_SCOPE,
            what + " must be a non-empty string, not " + repr(value),
        )
    if SEPARATOR in value:
        raise CoordinationError(
            RefusalReason.UNREGISTERED_SCOPE,
            what + " must not contain " + repr(SEPARATOR) + ", which is the field separator",
        )
    return value


def derive(prefix, *fields):
    """prefix + the digest of the fields that DEFINE the record, in the order given.

    What goes in decides what a replay converges on, so each caller states its own choice and
    says why beside the call rather than here.
    """
    return prefix + "-" + sha256_hex(SEPARATOR.join(fields))[:ID_WIDTH]


class Refusal:
    """A decided refusal, carried from validation to the one place that records it.

    Returned rather than raised, so the caller can write the contest inside the transaction
    that decided it. Raising at the decision point rolls that transaction back and takes the
    evidence with it, which is the opposite of retaining a contest.

    Public, unlike linkage's equivalent, because three modules construct one.
    """

    def __init__(self, reason, detail, *, domain, subject, incumbent="", challenger=""):
        self.reason = reason
        self.detail = detail
        self.domain = domain
        self.subject = subject
        self.incumbent = incumbent or ""
        self.challenger = challenger or ""

    def error(self):
        return CoordinationError(self.reason, self.detail)

    def to_record(self):
        return {
            "domain": self.domain, "subject": self.subject,
            "reason": self.reason.value, "detail": self.detail,
            "incumbent": self.incumbent or None, "challenger": self.challenger or None,
        }


class Conflicts:
    """Contested coordination attempts, retained after they were refused.

    One table for all three domains, so an operator reads every contest in one place instead
    of three. Written with ON CONFLICT DO UPDATE against a logical unique key, so a loser that
    retries converges on one row rather than accumulating one per attempt - and the key's two
    identity columns are NOT NULL because SQLite treats NULLs as distinct in a unique index,
    which would defeat exactly that.
    """

    def __init__(self, store):
        self.store = store

    def record_in(self, db, refusal, *, at):
        db.execute(
            "INSERT INTO coordination_conflicts (at, domain, subject, reason, incumbent,"
            " challenger, detail) VALUES (?,?,?,?,?,?,?)"
            " ON CONFLICT (domain, subject, reason, incumbent, challenger) DO UPDATE SET"
            " at = excluded.at, detail = excluded.detail",
            (at, refusal.domain, refusal.subject, refusal.reason.value,
             refusal.incumbent, refusal.challenger, refusal.detail),
        )

    def all(self, domain, subject):
        return [
            {
                "at": row["at"], "domain": row["domain"], "subject": row["subject"],
                "reason": row["reason"], "detail": row["detail"],
                "incumbent": row["incumbent"] or None,
                "challenger": row["challenger"] or None,
            }
            for row in self.store.all(
                "SELECT * FROM coordination_conflicts"
                "  WHERE domain = ? AND subject = ? ORDER BY id",
                (domain, subject),
            )
        ]
