"""Relay CLI operations against an isolated state directory."""

import asyncio
import importlib.util
import json
import os
import sqlite3
import stat
import subprocess
import sys
import tempfile
import threading
from concurrent.futures import Future
from contextlib import contextmanager, nullcontext
from pathlib import Path

from .core import ROOT


def resolve(value, results, tmp_path):
    if isinstance(value, dict):
        if "$step" in value:
            result = results[value["$step"]]
            for key in value.get("path", []):
                result = result[key]
            return result
        return {key: resolve(item, results, tmp_path) for key, item in value.items()}
    if isinstance(value, list):
        return [resolve(item, results, tmp_path) for item in value]
    if isinstance(value, str):
        return value.replace("${HOME}", str(tmp_path))
    return value


@contextmanager
def relay_host(given, tmp_path):
    """Serve the real relay transport on an ephemeral case-local unix socket."""
    from websockets.asyncio.server import unix_serve

    spec = importlib.util.spec_from_file_location(
        "relay_contract_host", ROOT / "packages/codex-thread-bridge/tests/conftest.py")
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    fake = module.FakeServer()
    fake.threads.update(given.get("threads", {}))
    for key, value in given.get("options", {}).items():
        setattr(fake, key, value)
    class FilteredSocket:
        """Apply the real App Server's archive and source filters to the bridge fake."""

        def __init__(self, ws):
            self.ws = ws
            self.request = ws.request
            self.listings = {}

        async def __aiter__(self):
            async for raw in self.ws:
                message = json.loads(raw)
                if message.get("method") == "thread/list":
                    self.listings[message["id"]] = message["params"]
                yield raw

        async def send(self, raw):
            message = json.loads(raw)
            params = self.listings.pop(message.get("id"), None)
            if params is not None and "result" in message:
                kinds = params.get("sourceKinds") or ["cli", "vscode"]
                threads = [thread for thread in fake.threads.values()
                           if bool(thread.get("archived")) == bool(params.get("archived"))
                           and thread.get("source", "cli") in kinds
                           and (not params.get("cwd") or thread.get("cwd") == params["cwd"])]
                start = int(params.get("cursor") or 0)
                end = start + params["limit"]
                message["result"] = {"data": [{"id": thread["id"]} for thread in threads[start:end]],
                                     "nextCursor": str(end) if end < len(threads) else None}
                raw = json.dumps(message)
            if message.get("id") is not None and "result" in message and "thread" in message["result"]:
                thread = message["result"]["thread"]
                if "environments" not in thread:
                    thread["environments"] = fake.threads[thread["id"]].get("environments", [])
                    raw = json.dumps(message)
            await self.ws.send(raw)

        async def close(self, *args, **kwargs):
            await self.ws.close(*args, **kwargs)

    async def handle(ws):
        await fake.handle(FilteredSocket(ws))

    socket_dir = tempfile.TemporaryDirectory(prefix="crw", dir="/tmp")
    socket = Path(socket_dir.name) / "host.sock"
    ready = Future()
    stopped = threading.Event()
    loop = asyncio.new_event_loop()

    async def serve():
        async with unix_serve(handle, str(socket)):
            ready.set_result(None)
            await asyncio.to_thread(stopped.wait)

    def start():
        asyncio.set_event_loop(loop)
        try:
            loop.run_until_complete(serve())
        except Exception as error:
            if not ready.done():
                ready.set_exception(error)
            else:
                raise
        finally:
            loop.close()

    thread = threading.Thread(target=start, daemon=True)
    thread.start()
    try:
        ready.result(timeout=10)
        yield fake, socket
    finally:
        stopped.set()
        thread.join(timeout=10)
        assert not thread.is_alive(), "relay host did not stop"
        socket_dir.cleanup()


def run(case, tmp_path):
    given = case.get("given", {})
    state = tmp_path / "state"
    for name, contents in given.get("files", {}).items():
        target = tmp_path / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(resolve(contents, {}, tmp_path), encoding="utf-8")
    if "sql" in given:
        state.mkdir(exist_ok=True)
        with sqlite3.connect(state / "relay.sqlite3") as connection:
            connection.executescript(given["sql"])
    if "sql_seed" in given:
        from codex_session_relay.store import Store
        store = Store(state / "relay.sqlite3")
        try:
            store.db.executescript(given["sql_seed"])
        finally:
            store.close()
    steps = case["run"].get("steps", [case["run"]])
    results = {}
    host_config = given.get("host")
    host_context = relay_host(host_config, tmp_path) if host_config is not None else nullcontext((None, None))
    with host_context as (fake, socket):
        for index, step in enumerate(steps):
            argv = resolve(step["argv"], results, tmp_path)
            overrides = resolve(step.get("env", {}), results, tmp_path)
            env = {**os.environ, **overrides,
                   "HOME": str(tmp_path),
                   "XDG_STATE_HOME": str(tmp_path / "xdg-state"),
                   "XDG_DATA_HOME": str(tmp_path / "xdg-data"),
                   "XDG_CONFIG_HOME": str(tmp_path / "xdg-config"),
                   "CODEX_HOME": str(tmp_path / "codex-home"),
                   "CODEX_SESSION_RELAY_SCOPE_DIR": str(tmp_path / "scopes"),
                   "PYTHONPATH": os.pathsep.join((str(ROOT / "packages/codex-session-relay/src"),
                                                   str(ROOT / "packages/codex-thread-bridge/src")))}
            payload = resolve(step.get("stdin"), results, tmp_path)
            command = [sys.executable, "-m", "codex_session_relay.cli", "--state", str(state)]
            if socket is not None:
                command.extend(("--socket", str(socket)))
            done = subprocess.run(
                [*command, *argv],
                input=json.dumps(payload) if isinstance(payload, (dict, list)) else payload,
                text=True, capture_output=True, check=False, timeout=step.get("timeout", 60),
                env=env, cwd=resolve(step.get("cwd", str(tmp_path)), results, tmp_path))
            text = done.stdout.strip()
            try:
                parsed = json.loads(text) if text else None
            except json.JSONDecodeError:
                parsed = None
            result = {"exit": done.returncode, "stdout": done.stdout, "stderr": done.stderr,
                      "stdout_json": parsed}
            results[step.get("id", str(index))] = result
        actual = {**result, "steps": results}
        if fake is not None:
            actual["host_calls"] = [[method, params] for method, params in fake.calls]
            actual["host_threads"] = fake.threads
    actual["files"] = {name: (tmp_path / name).exists() for name in case["expect"].get("files", {})}
    actual["observed"] = {
        name: {"exists": path.exists(),
               "bytes": path.read_bytes().hex() if path.is_file() else None,
               "size": path.stat().st_size if path.is_file() else None,
               "mode": stat.S_IMODE(path.stat().st_mode) if path.exists() else None,
               "target": os.readlink(path) if path.is_symlink() else None,
               "entries": sorted(child.name for child in path.iterdir()) if path.is_dir() else None}
        for name in case["expect"].get("observe", [])
        for path in [tmp_path / name]
    }
    if "queries" in case["expect"]:
        with sqlite3.connect(state / "relay.sqlite3") as connection:
            actual["sql"] = {name: [list(row) for row in connection.execute(query).fetchall()]
                             for name, query in case["expect"]["queries"].items()}
    return actual
