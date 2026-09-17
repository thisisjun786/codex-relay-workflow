"""OPS-2.1 four signals and OPS-2.2 five classes, in their fixed precedence.

The order is the rule, not an implementation detail. A recorded checkout carrying an
uncommitted change elsewhere still produces a matching package digest, so without
precedence it would read as 'own' and be reused, and the handling that exists to protect
the user's work would never run. 'own' is therefore the residual class.
"""

CONFLICT = "conflict"
FORK = "fork"
FOREIGN = "foreign"
UNMEASURED = "unmeasured"
OWN = "own"

CLASSES = (CONFLICT, FORK, FOREIGN, UNMEASURED, OWN)

# classify also answers UNREADABLE, which is not one of the five classes: it is the answer that
# no class was decided, because a signal could not be read. Naming it here rather than leaving
# it as a literal in the one place it is produced keeps the set of things classify can return
# readable from this module instead of from its body.
UNREADABLE = "unreadable"
ANSWERS = CLASSES + (UNREADABLE,)


class Judgement:
    """Signal values, and the readings that could not be made, collected in one place.

    A judgment cell carries the value its OWN question's reading produced, and nothing else. A
    reading that did not answer leaves the cell empty and names itself unreadable; it is never
    flattened into a boolean and never filled from a neighbouring question.

    The flattening is the defect this exists to stop, and it is quiet. None compared with a
    recorded string is False, so a git read nobody could perform arrived at classification as a
    tree that disagrees, and an installation this command owns was reported as somebody's fork.
    Writing the comparison out at each site is what let one of them answer correctly and the
    next one, three lines away, not.
    """

    def __init__(self):
        self.unreadable = []

    def compare(self, observed, recorded, *, what):
        """Whether an observation agrees with what the record names.

        Returns None when the observation was not made, and records why. Absence of evidence is
        not evidence of disagreement.
        """
        if observed is None:
            self.unreadable.append(what)
            return None
        return observed == recorded

    def answer(self, observed, *, what):
        """A reading that IS the signal rather than one compared against a record."""
        if observed is None:
            self.unreadable.append(what)
        return observed

    def note(self, what):
        """A signal that could not be read for a reason this collector did not observe."""
        self.unreadable.append(what)
        return None


class Signals:
    """What was measured, and what could not be.

    'unreadable' is not a list of failures to work around. Under OPS-2.1 a signal that
    cannot be read is not a signal that agrees, so any entry in it stops classification.

    'pointer_conflict' is here because the owned pointer took over a question the registration
    used to answer. Once the Codex registration names a stable pointer rather than an
    environment, LINKED says the configuration names the pointer and stops saying which runtime
    that is, so a pointer repointed by hand at somebody's fork would leave every other cell
    satisfied. The cell whose question that is now asks it.
    """

    def __init__(self, *, entry_point_recorded=None, commit_matches=None, tree_matches=None,
                 working_tree_clean=None, digest_matches=None, has_point=None,
                 registration_conflict=None, link_conflict=None, pointer_conflict=None,
                 unreadable=None):
        self.entry_point_recorded = entry_point_recorded
        self.commit_matches = commit_matches
        self.tree_matches = tree_matches
        self.working_tree_clean = working_tree_clean
        self.digest_matches = digest_matches
        self.has_point = has_point
        self.registration_conflict = registration_conflict
        self.link_conflict = link_conflict
        self.pointer_conflict = pointer_conflict
        self.unreadable = list(unreadable or [])


def classify(signals):
    """Return (class, reasons). The first matching class wins."""
    if signals.unreadable:
        return UNREADABLE, [
            "classification stopped: " + reading + " could not be read" for reading in signals.unreadable
        ]

    reasons = []
    conflicts = (signals.registration_conflict, signals.link_conflict,
                 signals.pointer_conflict)
    if any(conflicts):
        for conflict in conflicts:
            if conflict:
                reasons.append(conflict)
        return CONFLICT, reasons

    # fork before foreign: a recorded path that has diverged is the user's work, and it is
    # preserved rather than reported as somebody else's installation.
    if signals.entry_point_recorded:
        if signals.working_tree_clean is False:
            reasons.append("the checkout has uncommitted changes, so it is no longer the recorded revision")
        if signals.commit_matches is False:
            reasons.append("the checkout is on a different commit than the definition records")
        if signals.tree_matches is False:
            reasons.append("the checkout tree differs from the one the definition records")
        if signals.digest_matches is False:
            reasons.append("the installed bytes differ from the recorded digest")
        if reasons:
            return FORK, reasons

    if not signals.entry_point_recorded:
        reasons.append("the entry point resolves outside every recorded path")
        if signals.digest_matches is False:
            reasons.append("its bytes also differ from the recorded digest, so it is unverified")
        elif not signals.has_point:
            reasons.append("no recorded run covers the combination it runs under, so it is unmeasured")
        return FOREIGN, reasons

    if not signals.has_point:
        return UNMEASURED, [
            "everything about the installed bytes agrees, but no recorded run covers the"
            " combination it runs under, so it is preserved and not reused"
        ]

    return OWN, ["entry point, revision, cleanliness, digest and a measured point all agree"]


def reusable(classification):
    """Only 'own' may be reused for a host-required command (OPS-2.2, OPS-4.1)."""
    return classification == OWN
