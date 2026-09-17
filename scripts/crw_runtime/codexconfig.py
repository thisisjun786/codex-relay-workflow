"""Reading and writing the MCP server tables in a Codex config.toml.

tomllib is the reader. It is standard library from Python 3.11, which is every interpreter this
command actually runs on, and without it every non-empty configuration is refused rather than
approximated.

That decision has a history worth keeping. This module used to contain a hand-written reader,
and it produced eight defects across as many review rounds: delimiter counting, escape decoding,
dotted names, quoted keys, the three-quote sequence skipping tables, brackets inside quoted
names, Unicode line boundaries, and quoted member assignments. It was then replaced by a
deliberately narrow subset that refused everything it did not model, and that produced two more.
Approximating TOML is an open problem; refusing is a closed one.

The asymmetry is the safety argument, and it is why refusing is acceptable: the damaging failure
is not declining to read a file, it is concluding a server is absent when it is registered and
then appending a second definition of it.

Parsing correctly is still not reading a registration. A file where args is the string "ab"
parses cleanly and list() turns it into ["a", "b"], so the shape of what was parsed is validated
before anything is compared. And writing is checked the same way: the proposed content is read
back before it is written, because appending correct text to a correctly-read file still does
not make the result mean what it says.
"""

import re
import sys

from .text import text_prefix

BARE_KEY = re.compile(r"[A-Za-z0-9_-]+")

# The escapes a TOML basic string may carry, used when WRITING one. Reading is tomllib's job.
ESCAPES = {"b": "\b", "t": "\t", "n": "\n", "f": "\f", "r": "\r", '"': '"', "\\": "\\"}
ESCAPED = {value: "\\" + key for key, value in ESCAPES.items()}


class Unreadable(Exception):
    def __init__(self, detail):
        super().__init__(detail)
        self.detail = detail


class ConfigView:
    def __init__(self, servers, unreadable):
        self.servers = servers
        self.unreadable = list(unreadable)

    @property
    def readable(self):
        return not self.unreadable


def registration_view(mapping):
    """Validate mcp_servers and project it to the two fields this command compares.

    A correct parse says nothing about shape. Without this, args = "ab" reads as a string,
    list() turns it into ["a", "b"], and a malformed registration compares equal to a
    requested ["a", "b"] -- a right answer to the wrong question.

    The projection is deliberate and symmetric: command and args are what registration
    decides on, and a server carrying env or anything else is left alone rather than
    refused.
    """
    if not isinstance(mapping, dict):
        raise Unreadable("mcp_servers is a table of servers, found "
                         + type(mapping).__name__)
    view = {}
    for name, entry in mapping.items():
        if not isinstance(entry, dict):
            raise Unreadable("the registration for " + repr(name) + " is a table, found "
                             + type(entry).__name__)
        projected = {}
        if "command" in entry:
            if not isinstance(entry["command"], str):
                raise Unreadable(repr(name) + " has a command that is not a string, it is "
                                 + type(entry["command"]).__name__)
            projected["command"] = entry["command"]
        if "args" in entry:
            args = entry["args"]
            if not isinstance(args, list) or not all(isinstance(a, str) for a in args):
                raise Unreadable(repr(name) + " has args that are not a list of strings, they"
                                 " are " + type(args).__name__)
            projected["args"] = list(args)
        view[name] = projected
    return view


def python_needed():
    """What a refusal has to say so somebody can act on it."""
    return (
        "reading a Codex configuration needs tomllib, which is standard library from Python"
        " 3.11; this controller is running " + ".".join(str(p) for p in sys.version_info[:3])
        + " at " + sys.executable + ". Rerun runtime_install.py on 3.11 or newer. The"
        " controller's interpreter is not the runtime's: this command installs 3.11+ runtimes"
        " whatever it is started with, so it is the controller that is too old here, not"
        " anything it installed."
    )


def scan(text):
    """Read the mcp_servers registrations, or say why the file cannot be read.

    Without tomllib every non-empty configuration is unreadable. That is the whole design: a
    hand-written approximation of TOML produced eight defects in this module, and the last two
    came from the narrow fallback written to replace it. Refusing is a closed problem and
    approximating is not.
    """
    try:
        import tomllib
    except ImportError:
        if not text.strip():
            # Nothing to misread. A clean host can still be diagnosed.
            return ConfigView({}, [])
        return ConfigView({}, [python_needed()])
    try:
        parsed = tomllib.loads(text)
    except Exception as error:
        return ConfigView({}, ["this file is not readable TOML: " + type(error).__name__
                               + ": " + str(error)])
    try:
        return ConfigView(registration_view(parsed.get("mcp_servers", {})), [])
    except Unreadable as error:
        return ConfigView({}, [error.detail])


def cross_check(text, view):
    """Compare the reader against tomllib where it exists, and report a disagreement."""
    try:
        import tomllib
    except ImportError:
        return None
    try:
        parsed = tomllib.loads(text)
    except Exception as error:
        return "tomllib could not parse this file: " + type(error).__name__ + ": " + str(error)
    if not view.readable:
        return None
    expected = parsed.get("mcp_servers", {})
    if set(expected) != set(view.servers):
        return ("the reader saw servers " + repr(sorted(view.servers))
                + " but tomllib saw " + repr(sorted(expected)))
    for name, entry in view.servers.items():
        theirs = expected.get(name) or {}
        if "command" in entry and theirs.get("command") != entry["command"]:
            return ("the reader read " + name + " command " + repr(entry["command"])
                    + " but tomllib read " + repr(theirs.get("command")))
        # Arguments decide LINKED against CONFLICT exactly as the command does, so leaving
        # them out of the comparison leaves half the registration unchecked.
        if "args" in entry and list(theirs.get("args") or []) != list(entry["args"] or []):
            return ("the reader read " + name + " args " + repr(entry["args"])
                    + " but tomllib read " + repr(theirs.get("args")))
    return None


def quote(value):
    """Render a TOML basic string this reader can read back unchanged."""
    out = ['"']
    for char in str(value):
        if char in ESCAPED:
            out.append(ESCAPED[char])
        elif ord(char) < 0x20 or ord(char) == 0x7F:
            out.append("\\u%04X" % ord(char))
        else:
            out.append(char)
    out.append('"')
    return "".join(out)


_quote = quote


def key(name):
    """A table-name segment this reader and TOML both read back as this exact name.

    A bare key is written as itself; anything else is quoted. Written raw, a name containing a
    dot becomes a sub-table of another server, so the registration this command believes it
    made is not the one in the file, and the next run appends a second one.
    """
    return name if BARE_KEY.fullmatch(name) else quote(name)


def render(name, command, args):
    lines = ["[mcp_servers." + key(name) + "]", "command = " + quote(command)]
    if args:
        lines.append("args = [" + ", ".join(quote(a) for a in args) + "]")
    return "\n".join(lines) + "\n"


def register(text, name, command, args):
    """Return (new_text, outcome, detail) without ever rewriting an existing table.

    LINKED   the registration is already exactly this one; nothing is written.
    CREATED  it was absent; the table is appended and the result was checked before writing.
    CONFLICT a different command or argument list is registered, or appending would not mean
             what it says; nothing is written.

    Reading the file correctly is not enough to append to it correctly. A root
    mcp_servers = {} is a closed inline table that a following [mcp_servers.x] cannot extend,
    and under [[mcp_servers]] an appended table attaches to the last array element instead of
    to a root mapping. So the proposal is read back before it is written, and it has to carry
    the intended registration and leave every other one alone.
    """
    view = scan(text)
    if not view.readable:
        return text, "UNREADABLE", "; ".join(view.unreadable)

    args = list(args or [])
    if name in view.servers:
        existing = view.servers[name]
        if existing.get("command") == command and list(existing.get("args") or []) == args:
            return text, "LINKED", "already registered with this exact command and arguments"
        return text, "CONFLICT", (
            "registered as " + repr(existing.get("command")) + " with " + repr(existing.get("args"))
            + ", requested " + repr(command) + " with " + repr(args)
        )

    separator = "" if text == "" or text_prefix(text, "\n\n", at="end") else ("\n" if text_prefix(text, "\n", at="end") else "\n\n")
    proposal = text + separator + render(name, command, args)

    check = scan(proposal)
    if not check.readable:
        return text, "CONFLICT", (
            "appending this registration would produce a file that cannot be read, so nothing"
            " was written: " + "; ".join(check.unreadable)
        )
    written = check.servers.get(name)
    if written is None or written.get("command") != command \
            or list(written.get("args") or []) != args:
        return text, "CONFLICT", (
            "appending would not produce the intended registration: the file would read "
            + repr(written) + " for " + repr(name) + ", so nothing was written"
        )
    for other, entry in view.servers.items():
        if check.servers.get(other) != entry:
            return text, "CONFLICT", (
                "appending would change the registration of " + repr(other)
                + ", so nothing was written"
            )
    return proposal, "CREATED", "appended at the end of the file, and read back before writing"
