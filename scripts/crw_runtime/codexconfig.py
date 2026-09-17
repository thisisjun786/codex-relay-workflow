"""A deliberately small reader for the MCP server tables in a Codex config.toml.

This is not a TOML parser and does not try to be one. tomllib does not exist on the Python
this repository runs its own checks with, and a hand-written general parser would be a much
larger thing to get wrong than the one shape that actually matters here.

It models exactly one shape, [mcp_servers.<name>], and treats everything it does not model
as unreadable. That asymmetry is the whole safety argument: the damaging failure is not
refusing a file it could have read, it is concluding that a server is absent because its
registration was written in a form the scanner did not recognise, and then appending a
second definition of a server that was already there.

The reader walks characters rather than counting delimiters, because a quote only means
what it means outside a string. A single-quoted literal value can legitimately contain the
three-quote sequence that opens a multi-line string, and counting delimiters reads that as a
fence which never closes, silently skipping every table after it. A basic string also carries
escapes that have to be decoded before its value can be compared with anything.

A server's name is the FIRST segment after mcp_servers. Deeper segments are that server's
own sub-tables. A real configuration carries [mcp_servers.oracle.env] and
[mcp_servers.codex-thread-bridge.tools.create_thread], and reading either as a server name
would invent a server that does not exist and hide one that does.
"""

import re

HEADER = re.compile(r"^\s*\[([^\[\]]*)\]\s*$")
ARRAY_HEADER = re.compile(r"^\s*\[\[")
ASSIGNMENT = re.compile(r"^\s*([^\s=#]+)\s*=")
BARE_KEY = re.compile(r"[A-Za-z0-9_-]+")

# The escapes a TOML basic string may carry. Anything else is unmodelled, and an unmodelled
# escape is reported rather than guessed at.
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


def decode(raw, literal):
    """The value a string holds, with escapes resolved. Literal strings carry none."""
    if literal:
        return raw
    out = []
    index = 0
    while index < len(raw):
        char = raw[index]
        if char != "\\":
            out.append(char)
            index += 1
            continue
        index += 1
        if index >= len(raw):
            raise Unreadable("a string ending in a backslash")
        marker = raw[index]
        if marker in ESCAPES:
            out.append(ESCAPES[marker])
            index += 1
        elif marker in ("u", "U"):
            width = 4 if marker == "u" else 8
            digits = raw[index + 1:index + 1 + width]
            if len(digits) != width:
                raise Unreadable("a truncated unicode escape")
            try:
                out.append(chr(int(digits, 16)))
            except ValueError as error:
                raise Unreadable("an unreadable unicode escape") from error
            index += 1 + width
        else:
            raise Unreadable("a string escape this reader does not model: " + repr("\\" + marker))
    return "".join(out)


def consume(line, fence):
    """Walk one line, returning (code, values, fence).

    'code' is the line with every string blanked to spaces of the same width, so a header or
    assignment can be matched without a quoted value being mistaken for syntax. 'values' are
    the decoded strings in order. 'fence' is the multi-line delimiter still open, if any.
    """
    values = []
    code = []
    index = 0
    width = len(line)

    if fence is not None:
        closing = line.find(fence)
        if closing == -1:
            return "", values, fence
        index = closing + 3
        fence = None
        code.append(" " * index)

    while index < width:
        char = line[index]
        if char == "#":
            break
        if line.startswith('"""', index) or line.startswith("'''", index):
            delimiter = line[index:index + 3]
            closing = line.find(delimiter, index + 3)
            if closing == -1:
                return "".join(code), values, delimiter
            values.append(line[index + 3:closing])
            code.append(" " * (closing + 3 - index))
            index = closing + 3
            continue
        if char in ('"', "'"):
            literal = char == "'"
            end = index + 1
            while end < width:
                if not literal and line[end] == "\\":
                    end += 2
                    continue
                if line[end] == char:
                    break
                end += 1
            if end >= width:
                raise Unreadable("an unterminated string")
            values.append(decode(line[index + 1:end], literal))
            code.append(" " * (end + 1 - index))
            index = end + 1
            continue
        code.append(char)
        index += 1
    return "".join(code), values, fence


def _split_key(text):
    """Split a dotted TOML key into segments, honouring quoted segments and their escapes.

    A quoted segment is a string and carries the same escapes a quoted value does. Finding its
    close by the next raw quote ends a name one character into itself when the name contains
    an escaped quote, and returning the raw text leaves a name that never compares equal to
    the one that was written. Both produce a second registration of a server already there.
    """
    segments = []
    index = 0
    while index < len(text):
        while index < len(text) and text[index] in " \t":
            index += 1
        if index >= len(text):
            return None
        char = text[index]
        if char in "\"'":
            literal = char == "'"
            end = index + 1
            while end < len(text):
                if not literal and text[end] == "\\":
                    end += 2
                    continue
                if text[end] == char:
                    break
                end += 1
            if end >= len(text):
                return None
            try:
                segments.append(decode(text[index + 1:end], literal))
            except Unreadable:
                # A key this reader cannot decode is not a key it may guess at. The caller
                # decides whether an unreadable key under mcp_servers is fatal or ignorable.
                return None
            index = end + 1
        else:
            match = BARE_KEY.match(text, index)
            if not match:
                return None
            segments.append(match.group(0))
            index = match.end()
        while index < len(text) and text[index] in " \t":
            index += 1
        if index >= len(text):
            return segments
        if text[index] != ".":
            return None
        index += 1
    return segments


def scan(text):
    """Read the mcp_servers tables, or say why the file cannot be read."""
    servers = {}
    current = None
    section = None
    fence = None
    pending = None

    try:
        # str.splitlines splits on Unicode boundaries such as U+2028 and U+0085 that TOML
        # does not treat as line endings, which tears a value containing one in half and
        # reads the remainder as syntax. TOML's line ending is a newline, optionally
        # preceded by a carriage return.
        for number, line in enumerate(text.replace("\r\n", "\n").split("\n"), 1):
            inside = fence is not None
            code, values, fence = consume(line, fence)
            if inside:
                # This line began inside a multi-line string, so nothing in it is syntax
                # until that string closes. A table header appearing after the close is a
                # shape this reader does not model rather than one it may ignore.
                if fence is not None:
                    continue
                if "[" in code:
                    raise Unreadable("line " + str(number) + ": a table header sharing a line"
                                     " with the end of a multi-line string")

            if pending is not None:
                pending["values"].extend(values)
                if "]" in code:
                    if current is not None:
                        servers[current][pending["key"]] = pending["values"]
                    pending = None
                continue

            stripped = code.strip()
            if not stripped:
                continue

            if ARRAY_HEADER.match(code):
                if "mcp_servers" in line:
                    raise Unreadable("line " + str(number) + ": an array-of-tables header under"
                                     " mcp_servers, which this reader does not model")
                current, section = None, None
                continue

            # A header is recognised from the blanked code, which already knows where the
            # strings are, and its key is then sliced out of the ORIGINAL line by the match
            # span. Matching the raw line instead rejects a quoted name containing a bracket,
            # which tomllib accepts, so the table would be read as no server at all; parsing
            # the blanked interior instead would erase the name. The span is exact because
            # every blanked string keeps its original width.
            header = HEADER.match(code)
            if header:
                raw_key = line[header.start(1):header.end(1)]
                segments = _split_key(raw_key)
                if segments is None:
                    if "mcp_servers" in raw_key:
                        raise Unreadable("line " + str(number) + ": a table header under"
                                         " mcp_servers whose key this reader cannot read")
                    current, section = None, None
                    continue
                section = segments
                if segments and segments[0] == "mcp_servers" and len(segments) >= 2:
                    name = segments[1]
                    if len(segments) == 2:
                        if name in servers:
                            raise Unreadable("line " + str(number) + ": " + name
                                             + " is defined more than once")
                        servers[name] = {}
                        current = name
                    else:
                        servers.setdefault(name, {})
                        current = None
                else:
                    current = None
                continue

            assignment = ASSIGNMENT.match(code)
            if assignment:
                key = assignment[1]
                segments = _split_key(key)
                if segments and segments[0] == "mcp_servers":
                    raise Unreadable("line " + str(number) + ": a dotted or inline mcp_servers"
                                     " assignment, which this reader does not model")
                if section == ["mcp_servers"]:
                    raise Unreadable("line " + str(number) + ": a member assignment inside the"
                                     " mcp_servers table, which this reader does not model")
                if current is not None and key in ("command", "args"):
                    value = code.split("=", 1)[1]
                    if key == "args" and "[" in value and "]" not in value:
                        pending = {"key": key, "values": values}
                        continue
                    servers[current][key] = values[0] if key == "command" and values else values

        if fence is not None:
            raise Unreadable("the file ends inside an unterminated multi-line string")
        if pending is not None:
            raise Unreadable("the file ends inside an unterminated array value")
    except Unreadable as error:
        return ConfigView(servers, [error.detail])
    return ConfigView(servers, [])


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
    CREATED  it was absent; the table is appended at the end of the file.
    CONFLICT a different command or argument list is registered; nothing is written.
    """
    view = scan(text)
    if not view.readable:
        return text, "UNREADABLE", "; ".join(view.unreadable)
    disagreement = cross_check(text, view)
    if disagreement:
        return text, "UNREADABLE", disagreement

    args = list(args or [])
    if name in view.servers:
        existing = view.servers[name]
        if existing.get("command") == command and list(existing.get("args") or []) == args:
            return text, "LINKED", "already registered with this exact command and arguments"
        return text, "CONFLICT", (
            "registered as " + repr(existing.get("command")) + " with " + repr(existing.get("args"))
            + ", requested " + repr(command) + " with " + repr(args)
        )

    separator = "" if text == "" or text.endswith("\n\n") else ("\n" if text.endswith("\n") else "\n\n")
    return text + separator + render(name, command, args), "CREATED", "appended at the end of the file"
