"""Textual tests, declared as textual.

Identity decisions over paths and recorded identifiers go through resolved-part containment.
This module exists so the remaining prefix tests -- a shebang marker, a section delimiter, a
trailing newline, an installer output tag -- can say plainly that they are about characters,
and so the inventory can forbid a raw .startswith or .endswith everywhere else. That turns a
judgement call at every call site into a question with one right answer.

It cannot stop somebody routing a path comparison through here. The behavioural sibling-path
cases are what protect that; this makes the choice visible when they do.
"""


def text_prefix(value, prefix, *, at="start"):
    """A prefix or suffix test on TEXT, declared as such.

    Identity decisions over paths and recorded identifiers go through within() and compare
    resolved parts. This exists so the remaining tests -- a shebang marker, a section
    delimiter, a trailing newline, an installer output tag -- can say plainly that they are
    about characters and not about identity. The inventory then forbids a raw .startswith or
    .endswith anywhere in these modules, which is a question with one right answer instead of
    a judgement call at every call site.

    It cannot stop somebody routing a path comparison through here; the behavioural
    sibling-path cases are what protect that, and this makes the choice visible when they do.
    """
    value, prefix = str(value), str(prefix)
    return value.startswith(prefix) if at == "start" else value.endswith(prefix)
