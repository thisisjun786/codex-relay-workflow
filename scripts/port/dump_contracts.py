#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["mcp>=1.26,<2", "websockets>=15,<18"]
# ///
# ─── How to run ───
# 1. Install uv: curl -LsSf https://astral.sh/uv/install.sh | sh
# 2. Run: uv run --no-sync python scripts/port/dump_contracts.py [--check] [--cli-source PATH]
# 3. Or make executable and run: chmod +x scripts/port/dump_contracts.py && ./scripts/port/dump_contracts.py
# ──────────────────
"""Freeze contracts from the installed Python implementation, not from a handwritten list."""

from __future__ import annotations

import argparse
import ast
import importlib.util
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Final, TypeAlias

# The acceptance command invokes python3 directly; use the repository's locked uv
# environment for the MCP SDK and both editable Python packages.
if sys.prefix == sys.base_prefix:
    os.chdir(Path(__file__).resolve().parents[2])
    os.execvp("uv", ["uv", "run", "--no-sync", "python", *sys.argv])

import anyio
from codex_session_relay import cli, errors, service, stopadapter, store
from codex_thread_bridge import bridge, rpc
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

ROOT: Final = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scripts.crw_runtime import bridgerecord, completion, hostrecord, staging

TARGET: Final = ROOT / "contract/schema"
RELAY: Final = ROOT / "packages/codex-session-relay/src/codex_session_relay"
JSONValue: TypeAlias = str | int | float | bool | None | list["JSONValue"] | dict[str, "JSONValue"]
BRIDGE: Final = ROOT / "packages/codex-thread-bridge/src/codex_thread_bridge"


def source_tree(path: Path) -> ast.Module:
    """Parse the source being frozen, including optional scratch mutation inputs."""
    return ast.parse(path.read_text(encoding="utf-8"), filename=str(path))


def function(tree: ast.AST, name: str) -> ast.FunctionDef | ast.AsyncFunctionDef:
    """Find a named function in an AST, including class methods."""
    return next(node for node in ast.walk(tree)
                if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == name)


def string_expression(node: ast.AST) -> str:
    """Evaluate only adjacent literal SQL strings, never arbitrary source code."""
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        return string_expression(node.left) + string_expression(node.right)
    raise ValueError(f"nonliteral SQL expression: {ast.dump(node)}")


def calls_to(tree: ast.AST, method: str) -> list[ast.Call]:
    return [node for node in ast.walk(tree) if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute) and node.func.attr == method]


def parser_contract(source: Path) -> tuple[dict[str, JSONValue], int]:
    """Walk every parser node and preserve argparse's live action semantics."""
    module = cli
    if source.resolve() != Path(cli.__file__).resolve():
        spec = importlib.util.spec_from_file_location(
            "codex_session_relay._contract_cli_scratch", source,
        )
        if spec is None or spec.loader is None:
            raise RuntimeError(f"cannot load CLI source: {source}")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    parser = module.build_parser()
    count = 0

    def walk(node: argparse.ArgumentParser) -> dict[str, JSONValue]:
        nonlocal count
        flags = []
        children = {}
        for action in node._actions:
            if isinstance(action, argparse._SubParsersAction):
                # choices contains aliases as well as children; one node per parser object.
                for name, child in action.choices.items():
                    if name not in children:
                        children[name] = walk(child)
                        count += 1
                continue
            default = action.default
            if default is argparse.SUPPRESS:
                default = "==SUPPRESS=="
            flags.append({"name": action.dest, "flags": list(action.option_strings),
                          "type": getattr(action.type, "__name__", None),
                          "action": type(action).__name__, "default": default,
                          "required": action.required,
                          "choices": list(action.choices) if action.choices is not None else None,
                          "nargs": action.nargs})
        return {"flags": flags, "subcommands": children}

    return walk(parser), count


def parser_counts(source: Path, parser: dict[str, JSONValue], count: int) -> bool:
    """Account for loop expansions and helper invocations against AST call sites."""
    tree = source_tree(source)
    builder = function(tree, "build_parser")
    sites = calls_to(builder, "add_parser")
    extras = []
    for node in ast.walk(builder):
        if not isinstance(node, ast.For) or not isinstance(node.iter, (ast.Tuple, ast.List)):
            continue
        if not all(isinstance(item, ast.Constant) and isinstance(item.value, str)
                   for item in node.iter.elts):
            continue
        loop_sites = [call for stmt in node.body for call in calls_to(stmt, "add_parser")]
        if loop_sites:
            extras.append((node.lineno, len(node.iter.elts), len(loop_sites)))
    helper_sites = [call for call in sites
                    if any(isinstance(parent, ast.FunctionDef) and parent.name != "build_parser"
                           and call in calls_to(parent, "add_parser")
                           for parent in ast.walk(builder) if isinstance(parent, ast.FunctionDef))]
    # Helper is called once for each literal marker_command(name) in build_parser.
    helper_invocations = [call for call in ast.walk(builder) if isinstance(call, ast.Call)
                          and isinstance(call.func, ast.Name) and call.func.id == "marker_command"]
    loop_extras = sum(length * sites_in_loop for _, length, sites_in_loop in extras)
    loop_call_sites = sum(sites_in_loop for _, _, sites_in_loop in extras)
    helper_extras = len(helper_invocations) - len(helper_sites)
    top = parser["subcommands"]
    assert isinstance(top, dict)
    expected = len(sites) + loop_extras - loop_call_sites + helper_extras
    print(f"top-level subcommands={len(top)} tree nodes={count} "
          f"add_parser call sites={len(sites)}")
    for line, length, sites_in_loop in extras:
        print(f"loop line {line}: {length} names x {sites_in_loop} call site(s) "
              f"= {length * sites_in_loop} nodes")
    print(f"helper line {helper_sites[0].lineno if helper_sites else 'absent'} "
          f"marker_command: {len(helper_invocations)} invocations "
          f"from {len(helper_sites)} call site(s); "
          f"tree_nodes = call_sites + loop_extras - loop_call_sites + helper_extras "
          f"= {len(sites)} + {loop_extras} - {loop_call_sites} + {helper_extras} = {expected}")
    return count == expected and len(top) <= count


async def mcp_contract() -> dict[str, JSONValue]:
    """Ask the actual stdio MCP server for its SDK-serialized tool definitions."""
    with tempfile.TemporaryDirectory(prefix="crw-contract-mcp-") as home:
        env = {**os.environ, "HOME": home, "CODEX_HOME": home,
               "XDG_STATE_HOME": home}
        params = StdioServerParameters(
            command=sys.executable,
            args=["-m", "codex_thread_bridge.server", "--socket", str(Path(home) / "absent.sock"),
                  "--state-dir", str(Path(home) / "ledger")], env=env,
        )
        async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
            await session.initialize()
            tools = (await session.list_tools()).tools
            return {"tools": [tool.model_dump(by_alias=True, exclude_none=True, mode="json")
                              for tool in tools]}


def bridge_contract(bridge_source: Path) -> dict[str, JSONValue]:
    """Read the call-method literals and numeric predicates from their owning methods."""
    bridge_tree, rpc_tree = source_tree(bridge_source), source_tree(BRIDGE / "rpc.py")
    methods = {call.args[0].value for call in calls_to(bridge_tree, "call")
               if call.args and isinstance(call.args[0], ast.Constant)
               and isinstance(call.args[0].value, str)}
    methods.update(call.args[0].value for call in calls_to(rpc_tree, "_request")
                   if call.args and isinstance(call.args[0], ast.Constant)
                   and isinstance(call.args[0].value, str))
    methods.update(value.value for node in ast.walk(function(rpc_tree, "connect"))
                   if isinstance(node, ast.Dict)
                   for key, value in zip(node.keys, node.values)
                   if isinstance(key, ast.Constant) and key.value == "method"
                   and isinstance(value, ast.Constant) and isinstance(value.value, str))
    clamps = {}
    for method, names in (("list_threads", ("limit",)),
                          ("read_thread", ("limit", "max_text_chars")),
                          ("wait_thread", ("timeout_seconds",))):
        for test in ast.walk(function(bridge_tree, method)):
            if not isinstance(test, ast.Compare) or len(test.ops) != 2:
                continue
            left, middle, right = test.left, test.comparators[0], test.comparators[1]
            if (isinstance(middle, ast.Name) and middle.id in names
                    and isinstance(left, ast.Constant) and isinstance(right, ast.Constant)):
                bounds = [left.value, right.value]
                key = {"max_text_chars": "maxTextChars", "timeout_seconds": "waitSeconds"}.get(
                    middle.id, middle.id)
                if key in clamps and clamps[key] != bounds:
                    raise ValueError(f"inconsistent bridge clamp {key}")
                clamps[key] = bounds
    assert set(clamps) == {"limit", "maxTextChars", "waitSeconds"}, clamps
    connect = function(rpc_tree, "connect")
    close = next(keyword.value.value for call in ast.walk(connect)
                 if isinstance(call, ast.Call) and isinstance(call.func, ast.Name)
                 and call.func.id == "unix_connect" for keyword in call.keywords
                 if keyword.arg == "close_timeout" and isinstance(keyword.value, ast.Constant))
    return json.loads(json.dumps({
        "methods": sorted(methods), "refusedServerRequests": sorted(rpc.APPROVAL_METHODS),
        "maxFrameBytes": rpc.MAX_FRAME_BYTES,
        "phaseTimeoutsSeconds": vars(rpc.PhaseBounds.from_timeout(20)),
        "closeTimeoutSeconds": close, "refusedRequestsKept": rpc.REFUSED_REQUESTS_KEPT,
        "clamps": {**clamps, "detailTurns": bridge.DETAIL_TURNS,
                   "itemPage": bridge.ITEM_PAGE},
    }))


def literal_keys(node: ast.AST) -> list[str]:
    """Collect string keys from a producer's dict literals and subscript writes."""
    keys = []
    for entry in ast.walk(node):
        if isinstance(entry, ast.Dict):
            keys.extend(key.value for key in entry.keys
                        if isinstance(key, ast.Constant) and isinstance(key.value, str))
        if isinstance(entry, ast.Subscript) and isinstance(entry.slice, ast.Constant) \
                and isinstance(entry.slice.value, str) and isinstance(entry.ctx, ast.Store):
            keys.append(entry.slice.value)
    return list(dict.fromkeys(keys))


def load_scratch(source: Path, original: Path, name: str):
    """Import a mutated module under its original package, leaving production files untouched."""
    if source.resolve() == original.resolve():
        return sys.modules[name]
    spec = importlib.util.spec_from_file_location(name + "_contract_scratch", source)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load source: {source}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def settings_keys() -> list[str]:
    """Read the configuration producer plus fields explicitly consumed by complaints()."""
    source = source_tree(ROOT / "scripts/crw_runtime/completion.py")
    produced = literal_keys(function(source, "configuration"))
    validation = source_tree(RELAY / "stopadapter.py")
    complaints = function(validation, "complaints")
    consumed = [call.args[0].value for call in calls_to(complaints, "get")
                if call.args and isinstance(call.args[0], ast.Constant)
                and isinstance(call.args[0].value, str)]
    return list(dict.fromkeys([*produced, *consumed]))


def hook_contract(source: Path) -> dict[str, JSONValue]:
    """Probe each settings field at the complaints boundary and exercise the hook surface."""
    module = load_scratch(source, Path(stopadapter.__file__), stopadapter.__name__)
    tree = source_tree(source)
    identity = function(tree, "event_identity")
    identity_fields = list(dict.fromkeys(call.args[0].value for call in calls_to(identity, "get")
        if isinstance(call.func, ast.Attribute) and isinstance(call.func.value, ast.Name)
        and call.func.value.id == "stop"
        and call.args and isinstance(call.args[0], ast.Constant)
        and isinstance(call.args[0].value, str)))
    keys = settings_keys()
    base = {"configVersion": module.CONFIG_VERSION, "relayExecutable": "/contract/relay",
            "markerRoot": "/contract/markers", "mode": module.OBSERVE,
            "timeoutSeconds": module.DEFAULT_TIMEOUT_SECONDS}
    assert not module.complaints(base), module.complaints(base)
    required = [key for key in base if module.complaints({name: value for name, value in base.items()
                                                          if name != key})]
    hold = {**base, "mode": module.HOLD}
    hold_required = [key for key in keys if key not in hold and
                     module.complaints(hold) and not module.complaints({**hold, key: "contract"})]
    plugin = {**base, "owner": module.OWNER_PLUGIN,
              "adapterInterpreter": "/contract/python", "adapterEntryPoint": "/contract/hook"}
    plugin_required = [key for key in plugin if key not in base and
                       module.complaints({name: value for name, value in plugin.items() if name != key})]
    absolute = [key for key in keys if key not in ("mode", "owner") and
                not module.complaints({**base, key: "/contract/value"}) and
                module.complaints({**base, key: "relative-value"})]
    minimum = 0
    assert module.complaints({**base, "timeoutSeconds": minimum})
    assert not module.complaints({**base, "timeoutSeconds": 1})
    verdict = {"decision": module.BLOCK,
               "hook_output": {"decision": module.BLOCK, "reason": "contract probe",
                               "continue": True}}
    output = json.loads(module.hook_output(verdict))
    output["reason"] = "string"
    with tempfile.TemporaryDirectory(prefix="crw-contract-hook-") as home:
        env = {**os.environ, "HOME": home, "CODEX_HOME": home, "XDG_STATE_HOME": home}
        result = subprocess.run([sys.executable, "-m", "codex_session_relay.stopadapter"],
                                input=b"{}", capture_output=True, env=env, check=False)
    if result.stdout or result.stderr:
        raise RuntimeError("hook emitted unexpected output for an unconfigured Stop")
    launcher = source_tree(ROOT / "plugins/crw/wiring/crw_stop_hook.py")
    launcher_bounds = {target.id: entry.value.value for entry in launcher.body
                       if isinstance(entry, ast.Assign) and isinstance(entry.value, ast.Constant)
                       for target in entry.targets if isinstance(target, ast.Name)
                       and target.id in ("MARGIN_SECONDS", "MAX_SECONDS")}
    return json.loads(json.dumps({
            "stdin": {"type": "object", "identityFields": identity_fields},
            "stdout": {"emptyAllowed": not result.stdout, "block": output},
            "exit": result.returncode, "configName": module.CONFIG_NAME,
            "settingsKeys": keys,
            "launcher": launcher_bounds,
            "validation": {"configVersion": module.CONFIG_VERSION,
                           "modes": list(module.MODES), "owners": list(module.OWNERS),
                           "journalPolicies": list(module.JOURNAL_POLICIES),
                           "required": required, "holdRequires": hold_required,
                           "pluginRequires": plugin_required,
                           "adapterPluginTimeoutLessThan": module.LAUNCHER_CEILING_SECONDS,
                           "installerPluginTimeoutMaxInclusive": completion.MAX_PLUGIN_GUARD_SECONDS,
                           "timeoutSeconds": {"minExclusive": minimum,
                                              "maxInclusive": module.MAX_TIMEOUT_SECONDS},
                           "pathsMustBeAbsolute": absolute}}))


def host_keys(source: Path) -> dict[str, JSONValue]:
    """Read the empty-record producer and subsequent record writes."""
    module = load_scratch(source, Path(hostrecord.__file__), hostrecord.__name__)
    tree = source_tree(source)
    keys = list(module.empty(1))
    updater = function(tree, "update")
    keys.extend(entry.slice.value for entry in ast.walk(updater)
                if isinstance(entry, ast.Subscript) and isinstance(entry.ctx, ast.Store)
                and isinstance(entry.value, ast.Name) and entry.value.id == "record"
                and isinstance(entry.slice, ast.Constant) and isinstance(entry.slice.value, str))
    keys.extend(call.args[0].value for call in calls_to(updater, "setdefault")
                if isinstance(call.func, ast.Attribute)
                and isinstance(call.func.value, ast.Name) and call.func.value.id == "record"
                and call.args and isinstance(call.args[0], ast.Constant)
                and isinstance(call.args[0].value, str))
    component = function(tree, "component")
    component_keys = [call.args[0].value for call in calls_to(component, "setdefault")
                      if isinstance(call.func, ast.Attribute)
                      and isinstance(call.func.value, ast.Name)
                      and call.func.value.id == "entry" and call.args
                      and isinstance(call.args[0], ast.Constant)
                      and isinstance(call.args[0].value, str)]
    return json.loads(json.dumps({"file": module.RECORD_NAME,
                                  "keys": list(dict.fromkeys(keys)),
                                  "componentKeys": component_keys}))


def daemon_keys() -> list[str]:
    """Merge initial record keys with subsequent service updates and stop fields."""
    tree = source_tree(RELAY / "service.py")
    keys = literal_keys(function(tree, "new_record"))
    for call in calls_to(tree, "_note"):
        keys.extend(keyword.arg for keyword in call.keywords if keyword.arg is not None)
    for method in ("stop", "supervise"):
        try:
            body = function(tree, method)
        except StopIteration:
            continue
        keys.extend(entry.slice.value for entry in ast.walk(body)
                    if isinstance(entry, ast.Subscript) and isinstance(entry.ctx, ast.Store)
                    and isinstance(entry.slice, ast.Constant)
                    and isinstance(entry.slice.value, str)
                    and isinstance(entry.value, ast.Name) and entry.value.id == "cleared")
        keys.extend(keyword.arg for call in ast.walk(body) if isinstance(call, ast.Call)
                    and isinstance(call.func, ast.Name) and call.func.id == "dict"
                    for keyword in call.keywords if keyword.arg is not None)
    return list(dict.fromkeys(keys))


def sqlite_contract(source: Path) -> str:
    """Observe actual Store PRAGMAs and extract executed SQL in initialization order."""
    module = store
    if source.resolve() != Path(store.__file__).resolve():
        spec = importlib.util.spec_from_file_location("codex_session_relay._contract_store_scratch", source)
        if spec is None or spec.loader is None:
            raise RuntimeError(f"cannot load Store source: {source}")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    with tempfile.TemporaryDirectory(prefix="crw-contract-store-") as home:
        instance = module.Store(Path(home) / "relay.sqlite3", socket_path=str(Path(home) / "host.sock"))
        try:
            pragmas = {key: instance.db.execute(f"PRAGMA {key}").fetchone()[0]
                       for key in ("journal_mode", "synchronous", "foreign_keys")}
        finally:
            instance.close()
    tree = source_tree(source)
    initializer = function(tree, "__init__")
    statements = []
    for call in calls_to(initializer, "execute"):
        if (call.args and isinstance(call.args[0], (ast.Constant, ast.BinOp))):
            try:
                sql = string_expression(call.args[0])
            except ValueError:
                continue
            if sql.startswith("INSERT OR IGNORE INTO schema_meta"):
                key = re.search(r"VALUES \('([^']+)'", sql)
                if key is None:
                    raise ValueError(f"unrecognized schema_meta seed: {sql}")
                value = str(module.SCHEMA_VERSION) if key.group(1) == "version" else "<runtime-value>"
                statements.append(sql.replace("?", "'" + value + "'") + ";")
            elif sql.startswith("INSERT OR IGNORE INTO assignment_settlements"):
                statements.append(sql + ";")
    # AST traversal is not ordered; restore execution order, including the conditional socket seed.
    statements.sort(key=lambda statement: next(
        call.lineno for call in calls_to(initializer, "execute")
        if call.args and isinstance(call.args[0], (ast.Constant, ast.BinOp))
        and statement.startswith(string_expression(call.args[0]).split(" VALUES ")[0])))
    return "\n".join([f"-- SCHEMA_VERSION = {module.SCHEMA_VERSION}",
                      "-- Observed PRAGMAs after opening a temporary Store",
                      *(f"-- PRAGMA {key}={value}" for key, value in pragmas.items()),
                      module.DDL.rstrip(), "-- GUARD_INDEXES executed separately after the DDL",
                      *(statement + ";" for _, statement in module.GUARD_INDEXES),
                      "-- schema_meta seeds and assignment_settlements backfill (runtime values shown as placeholders)",
                      *statements]) + "\n"


def contracts(parser: dict[str, JSONValue], tools: dict[str, JSONValue],
              store_source: Path, bridge_source: Path, hook_source: Path,
              host_source: Path) -> dict[str, str]:
    """Collect source-derived contracts without opening a production database."""
    sql = sqlite_contract(store_source)
    hook_keys = settings_keys()
    data = {
        "relay-cli.json": parser,
        "relay-exit-codes.json": {
            "codes": {"ok": cli.EXIT_OK, "refused": cli.EXIT_REFUSED,
                      "host": cli.EXIT_HOST, "usage": cli.EXIT_USAGE,
                      "bound_spent": service.EXIT_BOUND_SPENT},
            "refusalReasons": {reason.name: reason.value for reason in errors.RefusalReason},
        },
        "bridge-mcp-tools.json": tools,
        "bridge-appserver.json": bridge_contract(bridge_source),
        "hook-io.json": hook_contract(hook_source),
        "records.json": {
            "completionHook": {"file": completion.CONFIG_NAME, "version": completion.CONFIG_VERSION,
                               "keys": hook_keys},
            "bridgeMcp": {"file": bridgerecord.RECORD_NAME,
                          "versions": list(bridgerecord.RECORD_VERSIONS),
                          "keys": list(bridgerecord.document(command="/contract/bridge"))
                                  + [bridgerecord.POLICY_FIELD],
                          "executionPolicyKeys": list(bridgerecord.POLICY_KEYS)},
            "hostRecord": host_keys(host_source),
            "stagingClaim": {"file": staging.CLAIM_NAME,
                             "keys": list(staging.claim_payload(staging.STAGING, issue="", run="")),
                             "version": staging.CLAIM_VERSION, "states": list(staging.CLAIM_STATES)},
            "daemon": {"file": service.DAEMON_RECORD, "keys": daemon_keys()},
        },
    }
    rendered = {name: json.dumps(value, indent=2, ensure_ascii=False) + "\n"
                for name, value in data.items()}
    rendered["relay-sqlite.sql"] = sql
    for path in sorted((RELAY / "schema").glob("*.json")):
        rendered[path.name] = path.read_text(encoding="utf-8")
    return rendered


def main() -> int:
    """Compare generated bytes to committed documents, or regenerate them."""
    options = argparse.ArgumentParser(description=__doc__)
    options.add_argument("--check", action="store_true")
    options.add_argument("--cli-source", type=Path, default=Path(cli.__file__))
    options.add_argument("--store-source", type=Path, default=Path(store.__file__))
    options.add_argument("--bridge-source", type=Path, default=Path(bridge.__file__))
    options.add_argument("--hook-source", type=Path, default=Path(stopadapter.__file__))
    options.add_argument("--host-source", type=Path, default=Path(hostrecord.__file__))
    args = options.parse_args()
    parser, count = parser_contract(args.cli_source)
    counts_match = parser_counts(args.cli_source, parser, count)
    documents = contracts(parser, anyio.run(mcp_contract), args.store_source,
                          args.bridge_source, args.hook_source, args.host_source)
    if args.check:
        changed = []
        for name, contents in sorted(documents.items()):
            path = TARGET / name
            if not path.exists() or path.read_text(encoding="utf-8") != contents:
                changed.append(name)
                print(f"DIFF {path}")
        extras = sorted(path.name for path in TARGET.iterdir() if path.name not in documents)
        for name in extras:
            print(f"EXTRA {TARGET / name}")
        return 1 if changed or extras or not counts_match else 0
    TARGET.mkdir(parents=True, exist_ok=True)
    for name, contents in documents.items():
        (TARGET / name).write_text(contents, encoding="utf-8")
    return 0 if counts_match else 1


if __name__ == "__main__":
    sys.exit(main())
