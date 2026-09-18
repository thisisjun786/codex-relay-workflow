"""Whether a request can have changed anything yet.

_mutate has to answer two different questions with two different words: nothing was attempted,
and something was attempted whose outcome is unknown. Neither can be read off the call site,
because the same statement is a preliminary read in one tool and a dispatch in another, and code
moves. The fact that settles it is when this process actually began an effect: the moment a
state-changing frame is handed to the socket, or a directory is about to exist. Both places
record it here, and _mutate only reads what was recorded.
"""

import contextlib
import contextvars

# Methods that only ask the host a question. Losing the answer to one of them cannot mean the
# host changed something. Everything else counts as a change, including a method nobody has
# classified yet: a mutation added later must not read as "nothing happened" because this set was
# never updated. thread/resume is deliberately absent. It carries cwd, model, sandbox and config,
# and that the host adopts none of them is an observation of one tested build, not a protocol
# guarantee this bridge is willing to spend a delivery claim on.
OBSERVATIONS = frozenset(
    {
        "initialize",
        "project/read",
        "thread/read",
        "thread/list",
        "thread/turns/list",
        "thread/items/list",
        "thread/goal/get",
    }
)


class Effects:
    """What one operation began, in the order it began it."""

    def __init__(self):
        self.attempted = []
        self.observed = []

    def sent(self, method: str):
        (self.observed if method in OBSERVATIONS else self.attempted).append(method)

    def local(self, effect: str):
        self.attempted.append(effect)


CURRENT = contextvars.ContextVar("codex_thread_bridge_effects", default=None)


def mark_sent(method: str):
    """Record a frame this process is about to write. The transport calls this, not its callers."""
    current = CURRENT.get()
    if current is not None:
        current.sent(method)


def mark_local(effect: str):
    """Record an effect this process is about to produce without asking the host."""
    current = CURRENT.get()
    if current is not None:
        current.local(effect)


@contextlib.contextmanager
def recording():
    """Collect one operation's effects. Anything outside a mutation records nothing.

    A context variable rather than an argument: every call inside the operation is covered,
    including one made by a helper or by a task it starts, and no call site can forget to opt in.
    Tasks and threads started here copy the mapping but share this object, so their marks land
    here too.
    """
    effects = Effects()
    token = CURRENT.set(effects)
    try:
        yield effects
    finally:
        CURRENT.reset(token)
