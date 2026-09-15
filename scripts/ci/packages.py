#!/usr/bin/env python3
"""Run both Python packages from this checkout and prove the run was real.

Three ways a package job passes without testing anything, in rough order of how
quietly they do it.

A command that collects nothing. `unittest discover` answers a wrong directory
with "Ran 0 tests ... OK" and exit 0, so a path that stops matching reports
success indefinitely. Every suite here runs under pytest, which exits 5 on an
empty collection, and the JUnit report is read back for a nonzero test count.

A skip standing in for a result. The relay skips its real-bridge seams when
`codex_thread_bridge` cannot be imported, which is precisely the integration this
repository now owns. Any skip fails this check, and that particular skip is named
in the failure so the cause is not guessed.

A pass earned by a different copy of the bridge. An interpreter that already has
`codex_thread_bridge` installed elsewhere satisfies the import without touching
`packages/codex-thread-bridge` at all, so both import locations are resolved and
checked against this checkout before any test runs.

Passing this check establishes that the packaged source builds, imports, tests and
exposes its CLI here. It is not evidence about an installed runtime, a live App
Server, or delivery on any host.
"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from xml.etree import ElementTree

ROOT = Path(__file__).resolve().parents[2]
BRIDGE_SKIP = "pinned bridge is not importable"

# directory under packages/, importable module, console script
PACKAGES = (
    ("codex-thread-bridge", "codex_thread_bridge", "codex-thread-bridge"),
    ("codex-session-relay", "codex_session_relay", "codex-session-relay"),
)


class Failure(Exception):
    """A check that did not hold. The message is the report."""


def run(command, *, cwd=ROOT, env=None, capture=False):
    where = "" if Path(cwd) == ROOT else f"  (in {Path(cwd).relative_to(ROOT)})"
    print("$ " + " ".join(str(part) for part in command) + where, flush=True)
    result = subprocess.run(
        [str(part) for part in command],
        cwd=str(cwd),
        env=env,
        text=True,
        stdout=subprocess.PIPE if capture else None,
    )
    if result.returncode != 0:
        raise Failure(f"{command[0]} exited {result.returncode}: {' '.join(map(str, command))}")
    return result.stdout or ""


def enclosing_checkout(path, stop=None):
    """The nearest ancestor of `path` that is a Git work tree, or None.

    `stop` bounds the walk and is never passed in production, which searches to the
    filesystem root. Tests supply it so the answer depends on a tree they built
    rather than on whether the host's temporary directory happens to be a checkout.
    """
    path = Path(path).resolve()
    boundary = Path(stop).resolve() if stop is not None else None
    for parent in (path,) + tuple(path.parents):
        if boundary is not None and parent == boundary:
            return None
        if (parent / ".git").exists():
            return parent
    return None


def temporary_root():
    """A scratch directory that is not inside a Git work tree.

    The bridge's worktree tests create repositories under the pytest basetemp. A
    surrounding repository changes what those tests observe, and on a host whose
    /tmp happens to be a checkout they fail for a reason that has nothing to do
    with the code under test.
    """
    override = os.environ.get("CRW_PACKAGES_TMPDIR")
    base = Path(override).expanduser() if override else Path(tempfile.gettempdir())
    base.mkdir(parents=True, exist_ok=True)
    base = base.resolve()
    checkout = enclosing_checkout(base)
    if checkout is not None:
        raise Failure(
            f"the temporary directory {base} is inside the Git work tree at {checkout}. "
            "Set CRW_PACKAGES_TMPDIR to a path outside any checkout."
        )
    return Path(tempfile.mkdtemp(prefix="crw-packages-", dir=str(base)))


def import_locations(env):
    """Resolve both modules in the synced environment and keep them in this checkout."""
    program = (
        "import json, codex_session_relay, codex_thread_bridge\n"
        "print(json.dumps({'codex_thread_bridge': codex_thread_bridge.__file__,"
        " 'codex_session_relay': codex_session_relay.__file__}))"
    )
    output = run(["uv", "run", "--no-sync", "python", "-c", program], env=env, capture=True)
    try:
        resolved = json.loads(output.strip().splitlines()[-1])
    except (IndexError, ValueError) as exc:
        raise Failure(f"could not read the import locations: {exc}") from exc
    for directory, module, _ in PACKAGES:
        expected = (ROOT / "packages" / directory / "src").resolve()
        actual = Path(resolved[module]).resolve()
        if not str(actual).startswith(str(expected) + os.sep):
            raise Failure(
                f"{module} imported from {actual}, which is outside {expected}. Another copy "
                "of this package is shadowing the one in this checkout."
            )
        print(f"  {module} -> {actual.relative_to(ROOT)}")


def read_report(path, directory):
    if not path.exists():
        raise Failure(f"{directory} produced no JUnit report; the suite did not run")
    root = ElementTree.parse(str(path)).getroot()
    suites = [root] if root.tag == "testsuite" else list(root)
    tests = failures = errors = 0
    skipped = []
    for suite in suites:
        tests += int(suite.get("tests", "0"))
        failures += int(suite.get("failures", "0"))
        errors += int(suite.get("errors", "0"))
        for case in suite.iter("testcase"):
            for skip in case.findall("skipped"):
                skipped.append(
                    (case.get("classname", ""), case.get("name", ""), skip.get("message", ""))
                )
    if tests == 0:
        raise Failure(f"{directory} collected no tests; an empty suite is not a passing suite")
    if failures or errors:
        raise Failure(f"{directory} reported {failures} failures and {errors} errors")
    for classname, name, message in skipped:
        print(f"  skipped {classname}::{name}: {message}", flush=True)
    if any(BRIDGE_SKIP in message for _, _, message in skipped):
        raise Failure(
            f"{directory} skipped a real-bridge test because codex_thread_bridge was not "
            "importable. Proving that import is the point of this check."
        )
    if skipped:
        raise Failure(f"{directory} skipped {len(skipped)} tests; this check requires a full run")
    return tests


def run_suite(directory, scratch, env):
    report = scratch / f"{directory}.xml"
    run(
        [
            "uv", "run", "--no-sync", "python", "-m", "pytest", "-q", "-rs",
            f"--basetemp={scratch / directory}",
            f"--junit-xml={report}",
        ],
        cwd=ROOT / "packages" / directory,
        env=env,
    )
    return read_report(report, directory)


def main():
    env = dict(os.environ)
    # The relay's own conformance gate skips itself unless this is set, and a skip is
    # not a result. The run stays offline: it validates packaged schemas and fixtures.
    env.setdefault("RELAY_CONFORMANCE_REQUIRED", "1")
    scratch = None
    try:
        run(["uv", "sync", "--locked", "--all-packages"], env=env)
        import_locations(env)
        scratch = temporary_root()
        counts = {name: run_suite(name, scratch, env) for name, _, _ in PACKAGES}
        for _, _, script in PACKAGES:
            run(["uv", "run", "--no-sync", script, "--help"], env=env)
        for directory, _, _ in PACKAGES:
            run(
                ["uv", "build", "--package", directory, "--out-dir", str(scratch / "dist")],
                env=env,
            )
    except Failure as exc:
        print(f"Packages check failed: {exc}", file=sys.stderr)
        return 1
    finally:
        if scratch is not None:
            shutil.rmtree(str(scratch), ignore_errors=True)
    print(
        "Packages check passed: "
        + ", ".join(f"{name} {count} tests" for name, count in counts.items())
        + ". Both CLIs responded and both wheels built. This says nothing about an installed"
        " runtime or a live host."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
