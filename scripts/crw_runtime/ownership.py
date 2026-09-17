"""OPS-2.1 four signals and OPS-2.2 five classes, in their fixed precedence.

The order is the rule, not an implementation detail. A recorded checkout carrying an
uncommitted change elsewhere still produces a matching package digest, so without
precedence it would read as 'own' and be reused, and the handling that exists to protect
the user's work would never run. 'own' is therefore the residual class.
"""

CLASSES = ("conflict", "fork", "foreign", "unmeasured", "own")


class Signals:
    """What was measured, and what could not be.

    'unreadable' is not a list of failures to work around. Under OPS-2.1 a signal that
    cannot be read is not a signal that agrees, so any entry in it stops classification.
    """

    def __init__(self, *, entry_point_recorded=None, commit_matches=None, tree_matches=None,
                 working_tree_clean=None, digest_matches=None, has_point=None,
                 registration_conflict=None, link_conflict=None, unreadable=None):
        self.entry_point_recorded = entry_point_recorded
        self.commit_matches = commit_matches
        self.tree_matches = tree_matches
        self.working_tree_clean = working_tree_clean
        self.digest_matches = digest_matches
        self.has_point = has_point
        self.registration_conflict = registration_conflict
        self.link_conflict = link_conflict
        self.unreadable = list(unreadable or [])


def classify(signals):
    """Return (class, reasons). The first matching class wins."""
    if signals.unreadable:
        return "unreadable", [
            "classification stopped: " + reading + " could not be read" for reading in signals.unreadable
        ]

    reasons = []
    if signals.registration_conflict or signals.link_conflict:
        for conflict in (signals.registration_conflict, signals.link_conflict):
            if conflict:
                reasons.append(conflict)
        return "conflict", reasons

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
            return "fork", reasons

    if not signals.entry_point_recorded:
        reasons.append("the entry point resolves outside every recorded path")
        if signals.digest_matches is False:
            reasons.append("its bytes also differ from the recorded digest, so it is unverified")
        elif not signals.has_point:
            reasons.append("no recorded run covers the combination it runs under, so it is unmeasured")
        return "foreign", reasons

    if not signals.has_point:
        return "unmeasured", [
            "everything about the installed bytes agrees, but no recorded run covers the"
            " combination it runs under, so it is preserved and not reused"
        ]

    return "own", ["entry point, revision, cleanliness, digest and a measured point all agree"]


def reusable(classification):
    """Only 'own' may be reused for a host-required command (OPS-2.2, OPS-4.1)."""
    return classification == "own"

