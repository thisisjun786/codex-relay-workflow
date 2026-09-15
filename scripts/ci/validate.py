#!/usr/bin/env python3
"""Validate this repository's supported metadata format, link paths and syntax.

This is intentionally a small structural check, not a general YAML/Markdown
parser or an assessment of skill behavior. Fenced examples are excluded from
link checks; fragments are not resolved.
"""

import ast
import json
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]
LINK = re.compile(r"\[[^\]\n]*\]\((<[^>\n]+>|[^\s)]+)(?:\s+\"[^\"]*\")?\)")


def scalar(text):
    text = text.strip()
    if text.startswith('"'):
        value = json.loads(text)
    elif text.startswith("'") and text.endswith("'"):
        value = text[1:-1].replace("''", "'")
    else:
        value = text
    if not isinstance(value, str) or not value.strip():
        raise ValueError("Expected a nonempty string scalar")
    return value


def metadata(path):
    text = path.read_text(encoding="utf-8")
    parts = text.split("---", 2)
    if len(parts) != 3 or parts[0].strip():
        raise ValueError("Missing frontmatter")
    fields = {}
    for line in parts[1].strip().splitlines():
        key, sep, value = line.partition(":")
        if not sep or key not in {"name", "description"} or key in fields:
            raise ValueError("Expected one name and one description field")
        fields[key] = scalar(value)
    if fields.get("name") != path.parent.name or not fields.get("description"):
        raise ValueError("Skill name must match its directory; description is required")
    if not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", fields["name"]):
        raise ValueError("Invalid skill name")
    ui = path.parent / "agents/openai.yaml"
    interface = {}
    section = None
    for line in ui.read_text(encoding="utf-8").splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if not line.startswith(" "):
            section = line.strip()
        elif section == "interface:":
            key, sep, value = line.strip().partition(":")
            if not sep or key in interface:
                raise ValueError("Malformed interface metadata")
            interface[key] = scalar(value)
    required = {"display_name", "short_description", "default_prompt"}
    if not required.issubset(interface):
        raise ValueError("Missing interface metadata")
    if "$" + fields["name"] not in interface["default_prompt"]:
        raise ValueError("Default prompt must name this skill")


def link_errors(path, root):
    errors = []
    fence = None
    for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        stripped = line.lstrip()
        marker = re.match(r"(`{3,}|~{3,})", stripped)
        if marker:
            delimiter = marker[1]
            if fence is None:
                fence = delimiter
            elif delimiter[0] == fence[0] and len(delimiter) >= len(fence):
                fence = None
            continue
        if fence:
            continue
        for match in LINK.finditer(line):
            target = match[1].strip("<>")
            url = urlsplit(target)
            if url.scheme or url.netloc or not url.path:
                continue
            destination = (path.parent / unquote(url.path)).resolve()
            if not destination.is_relative_to(root.resolve()) or not destination.exists():
                errors.append(f"{path.relative_to(root)}:{number}: invalid local link {target}")
    return errors


def main():
    files = subprocess.check_output(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], cwd=ROOT
    ).decode().split("\0")
    errors = []
    skills = 0
    for name in sorted(set(files) - {""}):
        path = ROOT / name
        try:
            if path.suffix == ".py":
                ast.parse(path.read_text(encoding="utf-8"), filename=name)
            if path.suffix == ".md":
                errors.extend(link_errors(path, ROOT))
            if path.name == "SKILL.md" and path.parent.parent == ROOT / "skills":
                metadata(path)
                skills += 1
        except (OSError, SyntaxError, ValueError) as exc:
            errors.append(f"{name}: {exc}")
    if not skills:
        errors.append("No skills validated")
    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    print(f"Validated {skills} skills, local link paths and Python syntax.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
