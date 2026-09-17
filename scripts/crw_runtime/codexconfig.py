"""A deliberately small reader for the MCP server tables in a Codex config.toml.

This is not a TOML parser and does not try to be one. tomllib does not exist on the Python
this repository runs its own checks with, and a hand-written general parser would be a much
larger thing to get wrong than the one shape that actually matters here.

It models exactly one shape, [mcp_servers.<name>], and treats everything it does not model
as unreadable. That asymmetry is the whole safety argument: the damaging failure is not
refusing a file it could have read, it is concluding that a server is absent because its
registration was written in a form the scanner did not recognise, and then appending a
second definition of a server that was already there.

A server's name is the FIRST segment after mcp_servers. Deeper segments are that server's
own sub-tables. A real configuration carries [mcp_servers.oracle.env] and
[mcp_servers.codex-thread-bridge.tools.create_thread], and reading either as a server name
would invent a server that does not exist and hide one that does.
"""

import re

HEADER = re.compile(r"^\s*\[([^\[\]]*)\]\s*(?:#.*)?$")
ARRAY_HEADER = re.compile(r"^\s*\[\[")
ASSIGNMENT = re.compile(r"^\s*([^\s=#]+)\s*=")
BARE_KEY = re.compile(r"[A-Za-z0-9_-]+")


class ConfigView:
    def __init__(self, servers, unreadable):
        self.servers = servers
        self.unreadable = list(unreadable)

    @property
    def readable(self):
        return not self.unreadable


def _split_key(text):
    """Split a dotted TOML key into segments, honouring quoted segments.

    Returns None when the key uses something this scanner does not model, which the caller
    turns into an unreadable finding rather than a guess.
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
            closing = text.find(char, index + 1)
            if closing == -1:
                return None
            segments.append(text[index + 1:closing])
            index = closing + 1
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


def _strings(text):
    """Every basic or literal string on one line, in order. Used for command and args."""
    found = []
    index = 0
    while index < len(text):
        char = text[index]
        if char == "#":
            break
        if char in "\"'":
            closing = text.find(char, index + 1)
            if closing == -1:
                return None
            found.append(text[index + 1:closing])
            index = closing + 1
            continue
        index += 1
    return found


def scan(text):
    """Read the mcp_servers tables, or say why the file cannot be read."""
    servers = {}
    unreadable = []
    current = None
    fence = None
    pending = None

    for number, line in enumerate(text.splitlines(), 1):
        if fence is not None:
            if fence in line:
                fence = None
            continue

        stripped = line.strip()

        if pending is not None:
            # Continuing a multi-line array value such as args = [ ... ].
            values = _strings(line)
            if values is None:
                unreadable.append("line " + str(number) + ": an unterminated string")
                return ConfigView(servers, unreadable)
            pending["values"].extend(values)
            if "]" in line:
                if current is not None:
                    servers[current][pending["key"]] = pending["values"]
                pending = None
            continue

        for marker in ('"""', "'''"):
            if stripped.count(marker) == 1:
                fence = marker
                break
        if fence is not None:
            continue

        if not stripped or stripped.startswith("#"):
            continue

        if ARRAY_HEADER.match(line):
            if "mcp_servers" in line:
                unreadable.append(
                    "line " + str(number) + ": an array-of-tables header under mcp_servers,"
                    " which this scanner does not model"
                )
                return ConfigView(servers, unreadable)
            current = None
            continue

        header = HEADER.match(line)
        if header:
            segments = _split_key(header[1])
            if segments is None:
                if "mcp_servers" in header[1]:
                    unreadable.append(
                        "line " + str(number) + ": a table header under mcp_servers whose key"
                        " this scanner cannot read"
                    )
                    return ConfigView(servers, unreadable)
                current = None
                continue
            if segments and segments[0] == "mcp_servers" and len(segments) >= 2:
                name = segments[1]
                if len(segments) == 2:
                    if name in servers:
                        unreadable.append(
                            "line " + str(number) + ": " + name + " is defined more than once"
                        )
                        return ConfigView(servers, unreadable)
                    servers[name] = {}
                    current = name
                else:
                    # A sub-table of that server, such as .env or .tools.create_thread.
                    # It belongs to the server; it is not another server.
                    servers.setdefault(name, {})
                    current = None
            else:
                current = None
            continue

        assignment = ASSIGNMENT.match(line)
        if assignment:
            key = assignment[1]
            segments = _split_key(key)
            if segments and segments[0] == "mcp_servers":
                unreadable.append(
                    "line " + str(number) + ": a dotted or inline mcp_servers assignment,"
                    " which this scanner does not model"
                )
                return ConfigView(servers, unreadable)
            if current is not None and key in ("command", "args"):
                value = line.split("=", 1)[1]
                values = _strings(value)
                if values is None:
                    unreadable.append("line " + str(number) + ": an unterminated string")
                    return ConfigView(servers, unreadable)
                if key == "args" and "[" in value and "]" not in value:
                    pending = {"key": key, "values": values}
                    continue
                servers[current][key] = values[0] if key == "command" and values else values

    if fence is not None:
        unreadable.append("the file ends inside an unterminated multi-line string")
    if pending is not None:
        unreadable.append("the file ends inside an unterminated array value")
    return ConfigView(servers, unreadable)


def cross_check(text, view):
    """Compare the scanner against tomllib where it exists, and report a disagreement.

    This protects the interpreters that have tomllib. It cannot protect the one the
    repository's own checks run on, which is why the refusal cases above have their own
    negative fixtures rather than relying on this.
    """
    try:
        import tomllib
    except ImportError:
        return None
    try:
        parsed = tomllib.loads(text)
    except Exception as error:
        return "tomllib could not parse this file: " + type(error).__name__ + ": " + error.__str__()
    if not view.readable:
        return None
    expected = set(parsed.get("mcp_servers", {}))
    if expected != set(view.servers):
        return (
            "the scanner read servers " + repr(sorted(view.servers))
            + " but tomllib read " + repr(sorted(expected))
        )
    return None


def render(name, command, args):
    lines = ["[mcp_servers." + name + "]", "command = " + _quote(command)]
    if args:
        lines.append("args = [" + ", ".join(_quote(a) for a in args) + "]")
    return "\n".join(lines) + "\n"


def _quote(value):
    return '"' + str(value).replace("\\", "\\\\").replace('"', '\\"') + '"'


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

