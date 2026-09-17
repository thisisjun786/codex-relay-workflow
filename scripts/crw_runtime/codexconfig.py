"""Reading the MCP server tables in a Codex config.toml, and refusing what cannot be read.

tomllib is the reader wherever it exists, which is Python 3.11 and newer. That is every place
this code actually runs: the host interpreter and the runtimes this command installs. A
hand-written TOML reader is an open correctness problem -- this module lost eight review rounds
to delimiter counting, escape decoding, dotted names, quoted keys, the three-quote sequence,
brackets inside quoted names, Unicode line boundaries and quoted member assignments -- and the
way to close it is to stop writing one.

A narrow fallback remains for the one CI job on 3.10. Its obligation is not to be a TOML parser.
It models command as a string and args as a list of strings, and it REFUSES every construct that
would make either of them anything else, plus any line it cannot classify. So outside its subset
the worst case is a refusal, never a different answer.

That asymmetry is the whole safety argument, and it is the same one either reader serves: the
damaging failure is not refusing a file that could have been read, it is concluding a server is
absent when it is registered, and then appending a second definition of it.

Parsing correctly is not the same as reading a registration. A file where args is the string
"ab" parses fine and list() turns it into ["a", "b"], so the shape of what was parsed is
validated before anything is compared.
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


def scan(text):
    """Read the mcp_servers registrations, or say why the file cannot be read."""
    try:
        import tomllib
    except ImportError:
        return scan_subset(text)
    try:
        parsed = tomllib.loads(text)
    except Exception as error:
        return ConfigView({}, ["this file is not readable TOML: " + type(error).__name__
                               + ": " + str(error)])
    try:
        return ConfigView(registration_view(parsed.get("mcp_servers", {})), [])
    except Unreadable as error:
        return ConfigView({}, [error.detail])


def _depth(code):
    """How far a value is left open by this line, counted on the blanked code."""
    return (code.count("[") + code.count("{")) - (code.count("]") + code.count("}"))


def _residue(code):
    """What is left of a value once strings, array syntax and whitespace are removed.

    consume blanks every string to spaces, so anything still here is a token this reader does
    not model: a bare word, a number, a boolean, a date. Without this, command = /c decodes to
    no strings at all and is recorded as an empty value, which is a different answer rather
    than a refusal.
    """
    for character in "[],{}":
        code = code.replace(character, " ")
    return code.strip()


def _assignment(line, code):
    """(key segments, value code) for an assignment, or None.

    The key is sliced from the ORIGINAL line, because a quoted key is blanked in the code and
    a regex over the code cannot see it. That blanking is exactly how a quoted member of the
    parent table slipped through unread and unrefused.
    """
    index, depth = None, 0
    for position, char in enumerate(code):
        if char in "[{":
            depth += 1
        elif char in "]}":
            depth -= 1
        elif char == "=" and depth == 0:
            index = position
            break
    if index is None:
        return None
    key_text = line[:index].strip()
    if not key_text:
        return None
    segments = _split_key(key_text)
    if segments is None:
        raise Unreadable("an assignment whose key this reader cannot read: " + repr(key_text))
    return segments, code[index + 1:]


MODELLED_FIELDS = ("command", "args")
TRIPLE_BASIC = chr(34) * 3
TRIPLE_LITERAL = chr(39) * 3


def scan_subset(text):
    """The 3.10 fallback: a small modelled subset, and a refusal for everything else.

    Kept as a named function rather than an implementation detail so the checks can compare it
    against tomllib on an interpreter that HAS tomllib. Otherwise the only job that exercises
    it is the only job with no oracle to judge it.
    """
    servers = {}
    current = None
    section = None
    fence = None
    pending = None
    depth = 0

    try:
        # str.splitlines splits on Unicode boundaries such as U+2028 that TOML does not treat
        # as line endings, which tears a value in half and reads the remainder as syntax.
        for number, line in enumerate(text.replace("\r\n", "\n").split("\n"), 1):
            where = "line " + str(number) + ": "
            inside = fence is not None
            code, values, fence = consume(line, fence)
            if inside:
                if fence is not None:
                    continue
                if "[" in code:
                    raise Unreadable(where + "a table header sharing a line with the end of a"
                                             " multi-line string")

            if depth > 0:
                # A value left open on an earlier line. Classified, because refusing every
                # continuation would refuse ordinary configurations.
                if pending is not None:
                    if _residue(code):
                        raise Unreadable(where + "a value this reader does not model in "
                                         + pending["key"] + ": " + repr(_residue(code)[:40]))
                    pending["values"].extend(values)
                depth += _depth(code)
                if depth <= 0:
                    depth = 0
                    if pending is not None:
                        if current is not None:
                            servers[current][pending["key"]] = pending["values"]
                        pending = None
                continue

            stripped = code.strip()
            if not stripped:
                continue

            if ARRAY_HEADER.match(code):
                header = HEADER.match(code.replace("[[", "[", 1).replace("]]", "]", 1))
                segments = _split_key(line[2:line.rindex("]]")]) if "]]" in line else None
                if segments and segments[0] == "mcp_servers":
                    raise Unreadable(where + "an array-of-tables under mcp_servers, which this"
                                             " reader does not model")
                current, section = None, None
                continue

            # A header is recognised from the blanked code, which knows where the strings are,
            # and its key is sliced from the original line by the match span, so a bracket
            # inside a quoted name is an ordinary character.
            header = HEADER.match(code)
            if header:
                raw_key = line[header.start(1):header.end(1)]
                segments = _split_key(raw_key)
                if segments is None:
                    if "mcp_servers" in raw_key:
                        raise Unreadable(where + "a table header under mcp_servers whose key"
                                                 " this reader cannot read")
                    current, section = None, None
                    continue
                section = segments
                if segments[0] == "mcp_servers" and len(segments) >= 2:
                    name = segments[1]
                    if len(segments) == 2:
                        if name in servers:
                            raise Unreadable(where + name + " is defined more than once")
                        servers[name] = {}
                        current = name
                    else:
                        if segments[2] in MODELLED_FIELDS:
                            # Defines command or args as a table. This reader models them as a
                            # string and a list of strings, and a projection that simply
                            # omitted this would compare equal to a registration that is not
                            # there.
                            raise Unreadable(where + "a table that defines "
                                             + segments[2] + " of " + name + " as a table")
                        servers.setdefault(name, {})
                        current = None
                else:
                    current = None
                continue

            found = _assignment(line, code)
            if found is None:
                raise Unreadable(where + "a line this reader cannot classify: "
                                 + repr(line.strip()[:60]))
            segments, value_code = found
            if segments[0] == "mcp_servers":
                raise Unreadable(where + "a dotted or inline mcp_servers assignment, which"
                                         " this reader does not model")
            if section == ["mcp_servers"]:
                raise Unreadable(where + "a member assignment inside the mcp_servers table,"
                                         " which this reader does not model")
            if current is not None and segments[0] in MODELLED_FIELDS:
                if len(segments) > 1:
                    raise Unreadable(where + "a dotted assignment that defines "
                                     + segments[0] + " of " + current + " as a table")
                if TRIPLE_BASIC in line or TRIPLE_LITERAL in line:
                    raise Unreadable(where + "a multi-line string value for "
                                     + segments[0] + ", which this reader does not model")
                if "{" in value_code:
                    raise Unreadable(where + "an inline table as the value of " + segments[0])
                residue = _residue(value_code)
                if residue:
                    raise Unreadable(where + segments[0] + " has a value this reader does not"
                                     " model: " + repr(residue[:40]))
                # The value's KIND has to match the field, or the fallback would read a
                # string as a one-element list and call a malformed registration equal to a
                # requested one. tomllib refuses that; so does this.
                is_array = "[" in value_code
                if segments[0] == "args" and not is_array:
                    raise Unreadable(where + "args is a list of strings, and this value is"
                                             " not a list")
                if segments[0] == "command" and is_array:
                    raise Unreadable(where + "command is a string, and this value is a list")
                if segments[0] == "command" and len(values) != 1:
                    raise Unreadable(where + "command is one string, and this value is "
                                     + str(len(values)))
                opened = _depth(value_code)
                if opened > 0:
                    pending = {"key": segments[0], "values": values}
                    depth = opened
                    continue
                servers[current][segments[0]] = (
                    values[0] if segments[0] == "command" and values else values
                )
                continue
            opened = _depth(value_code)
            if opened > 0:
                depth = opened

        if fence is not None:
            raise Unreadable("the file ends inside an unterminated multi-line string")
        if depth > 0:
            raise Unreadable("the file ends inside an unterminated value")
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

    separator = "" if text == "" or text.endswith("\n\n") else ("\n" if text.endswith("\n") else "\n\n")
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
