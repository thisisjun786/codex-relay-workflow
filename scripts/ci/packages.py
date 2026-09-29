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
# Seconds per test module, by package directory then module path as pytest names it.
# Shards are balanced on it; `CRW_PACKAGES_RECORD` on a whole run regenerates it.
DURATIONS = Path(__file__).resolve().with_name("package-durations.json")

# directory under packages/, importable module, console script
PACKAGES = (
    ("codex-thread-bridge", "codex_thread_bridge", "codex-thread-bridge"),
    ("codex-session-relay", "codex_session_relay", "codex-session-relay"),
)


# Loaded into pytest with `-p` when a CI leg runs one shard. Whole test modules are
# assigned to shards, heaviest first onto the lightest shard (ties go to the lower
# shard, and equal weights are taken in name order), so a module's tests (and any
# module-level test that checks what its siblings recorded) stay together. A module
# weighs the seconds DURATIONS recorded for it; a module the table does not name yet
# weighs its collected tests at the mean seconds per test of the recorded modules in
# this collection, or 1 per test when none of them is recorded. Every leg computes the
# same assignment from the same collection and table, so shards 1..N are disjoint and
# together hold every collected test. Deselected items are reported as such, never as
# skips, and each leg still refuses an empty or skipped run.
SHARD_PLUGIN = """\
import json
import os


def pytest_collection_modifyitems(config, items):
    index, total = (int(part) for part in os.environ["CRW_PACKAGES_SHARD"].split("/"))
    recorded = json.loads(os.environ.get("CRW_PACKAGES_DURATIONS") or "{}")
    counts = {}
    for item in items:
        module = item.nodeid.split("::", 1)[0]
        counts[module] = counts.get(module, 0) + 1
    known = [module for module in counts if module in recorded]
    known_tests = sum(counts[module] for module in known)
    per_test = sum(recorded[module] for module in known) / known_tests if known_tests else 1
    weight = {
        module: recorded[module] if module in recorded else counts[module] * per_test
        for module in counts
    }
    loads, owner = [0] * total, {}
    for module in sorted(counts, key=lambda name: (-weight[name], name)):
        shard = loads.index(min(loads))
        owner[module] = shard
        loads[shard] += weight[module]
    keep, drop = [], []
    for item in items:
        (keep if owner[item.nodeid.split("::", 1)[0]] == index - 1 else drop).append(item)
    if drop:
        config.hook.pytest_deselected(items=drop)
    items[:] = keep
"""
SHARD_MODULE = "crw_packages_shard"


class Failure(Exception):
    """A check that did not hold. The message is the report."""


def parse_shard(value):
    """`K/N` with 1 <= K <= N, as (K, N). The whole run is 1/1."""
    index, slash, total = value.partition("/")
    if slash and index.isdecimal() and total.isdecimal() and 1 <= int(index) <= int(total):
        return int(index), int(total)
    raise Failure(f"--shard must be K/N with 1 <= K <= N, not {value!r}")


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


def load_durations(path=DURATIONS):
    """The recorded table: {package directory: {module path: seconds}}."""
    try:
        table = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise Failure(f"could not read the duration table {path}: {exc}") from exc
    if not isinstance(table, dict) or not all(
        isinstance(modules, dict)
        and all(type(seconds) in (int, float) and seconds >= 0 for seconds in modules.values())
        for modules in table.values()
    ):
        raise Failure(f"{path} must map each package directory to {{module path: seconds}}")
    return table


def module_durations(report, package):
    """Seconds per test module from one JUnit report, rounded to 0.1.

    pytest writes a case's classname as its node id with `/` and `::` turned into dots
    and `.py` dropped, so the module is the longest dotted prefix that names a file
    under the package directory. The key is that file's path, as the shard plugin
    reads it from the node id.
    """
    totals = {}
    for case in ElementTree.parse(str(report)).getroot().iter("testcase"):
        classname = case.get("classname", "")
        parts = classname.split(".")
        candidates = ("/".join(parts[:end]) + ".py" for end in range(len(parts), 0, -1))
        module = next((path for path in candidates if (Path(package) / path).is_file()), None)
        if module is None:
            raise Failure(f"no test module under {package} matches the classname {classname!r}")
        totals[module] = totals.get(module, 0.0) + float(case.get("time") or 0)
    return {module: round(seconds, 1) for module, seconds in totals.items()}


def write_durations(path, table):
    text = json.dumps(table, indent=1, sort_keys=True) + "\n"
    try:
        Path(path).expanduser().write_text(text, encoding="utf-8")
    except OSError as exc:
        raise Failure(f"could not write the duration table {path}: {exc}") from exc


def run_suite(directory, scratch, env, shard=(1, 1), durations=None):
    """Run one package's suite (or its shard) and return (tests, JUnit report path)."""
    report = scratch / f"{directory}.xml"
    command = [
        "uv", "run", "--no-sync", "python", "-m", "pytest", "-q", "-rs",
        f"--basetemp={scratch / directory}",
        f"--junit-xml={report}",
    ]
    if shard != (1, 1):
        plugins = scratch / "plugins"
        plugins.mkdir(exist_ok=True)
        (plugins / f"{SHARD_MODULE}.py").write_text(SHARD_PLUGIN)
        env = dict(env)
        env["PYTHONPATH"] = os.pathsep.join(filter(None, [str(plugins), env.get("PYTHONPATH")]))
        env["CRW_PACKAGES_SHARD"] = f"{shard[0]}/{shard[1]}"
        # The plugin runs in the package's environment, which cannot import this script.
        env["CRW_PACKAGES_DURATIONS"] = json.dumps((durations or {}).get(directory, {}))
        command += ["-p", SHARD_MODULE]
    run(command, cwd=ROOT / "packages" / directory, env=env)
    return read_report(report, directory), report


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    env = dict(os.environ)
    # The relay's own conformance gate skips itself unless this is set, and a skip is
    # not a result. The run stays offline: it validates packaged schemas and fixtures.
    env.setdefault("RELAY_CONFORMANCE_REQUIRED", "1")
    record = env.get("CRW_PACKAGES_RECORD")
    scratch = None
    try:
        if argv[:1] == ["--shard"] and len(argv) == 2:
            shard = parse_shard(argv[1])
        elif not argv:
            shard = (1, 1)
        else:
            raise Failure("usage: packages.py [--shard K/N]")
        if record and shard != (1, 1):
            raise Failure("CRW_PACKAGES_RECORD needs the whole run; a shard times only its slice")
        durations = load_durations() if shard != (1, 1) else {}
        run(["uv", "sync", "--locked", "--all-packages"], env=env)
        import_locations(env)
        scratch = temporary_root()
        counts, recorded = {}, {}
        for name, _, _ in PACKAGES:
            counts[name], report = run_suite(name, scratch, env, shard, durations)
            if record:
                recorded[name] = module_durations(report, ROOT / "packages" / name)
        for _, _, script in PACKAGES:
            run(["uv", "run", "--no-sync", script, "--help"], env=env)
        for directory, _, _ in PACKAGES:
            run(
                ["uv", "build", "--package", directory, "--out-dir", str(scratch / "dist")],
                env=env,
            )
        if record:
            write_durations(record, recorded)
            print(f"Recorded the per-module durations of both suites in {record}")
    except Failure as exc:
        print(f"Packages check failed: {exc}", file=sys.stderr)
        return 1
    finally:
        if scratch is not None:
            shutil.rmtree(str(scratch), ignore_errors=True)
    part = "" if shard == (1, 1) else f" (shard {shard[0]}/{shard[1]})"
    print(
        f"Packages check passed{part}: "
        + ", ".join(f"{name} {count} tests" for name, count in counts.items())
        + ". Both CLIs responded and both wheels built. This says nothing about an installed"
        " runtime or a live host."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
