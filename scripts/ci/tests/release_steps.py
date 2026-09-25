"""Extract named literal shell blocks from the release workflow.

The tests execute those blocks. This module does not decide whether a release
is valid.
"""

from pathlib import Path
import re


WORKFLOW = Path(__file__).resolve().parents[3] / ".github" / "workflows" / "release.yml"

_JOB = re.compile(r"(?m)^  (?P<name>[a-z][a-z0-9-]*):\n")
_STEP = re.compile(r"(?m)^      - id: (?P<name>[a-z][a-z0-9-]+)\n")
_RUN = re.compile(r"(?m)^        run: \|\n(?P<body>(?:          .*\n|\n)*)")


def workflow_text(path=WORKFLOW):
    return Path(path).read_text(encoding="utf-8")


def job_body(name, text=None):
    source = workflow_text() if text is None else text
    matches = list(_JOB.finditer(source))
    found = [match for match in matches if match.group("name") == name]
    if len(found) != 1:
        raise ValueError(f"expected one {name} job, found {len(found)}")
    start = found[0].end()
    later = [match.start() for match in matches if match.start() > found[0].start()]
    end = later[0] if later else len(source)
    return source[start:end]


def step_block(job, step, text=None):
    body = job_body(job, text)
    matches = list(_STEP.finditer(body))
    found = [match for match in matches if match.group("name") == step]
    if len(found) != 1:
        raise ValueError(f"expected one {step} step, found {len(found)}")
    start = found[0].end()
    later = [match.start() for match in matches if match.start() > found[0].start()]
    # A following step may omit an id. Stop at the next step list item too.
    next_item = re.search(r"(?m)^      - ", body[start:])
    candidates = []
    if later:
        candidates.append(later[0])
    if next_item:
        candidates.append(start + next_item.start())
    end = min(candidates) if candidates else len(body)
    section = body[start:end]
    run = _RUN.search(section)
    if run is None or section[run.end():].strip():
        raise ValueError(f"expected one literal shell block on {step}")
    script = re.sub(r"^          ", "", run.group("body"), flags=re.M)
    metadata = section[:run.start()]
    return {"metadata": metadata, "script": script}
