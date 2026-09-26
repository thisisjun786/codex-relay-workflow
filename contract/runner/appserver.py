"""Scripted App Server over a real unix websocket socket."""

import asyncio
import tempfile
from contextlib import asynccontextmanager
from pathlib import Path


@asynccontextmanager
async def fake_host(given, tmp_path):
    import importlib.util

    from websockets.asyncio.server import unix_serve
    spec = importlib.util.spec_from_file_location(
        "bridge_fakehost_fixture", Path(__file__).resolve().parents[2] /
        "packages/codex-thread-bridge/tests/conftest.py")
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    fake = module.FakeServer()
    for key, value in given.get("host", {}).items():
        setattr(fake, key, value)
    with tempfile.TemporaryDirectory(prefix="crw-", dir="/dev/shm") as short:
        socket = Path(short) / "h.sock"
        async with unix_serve(fake.handle, str(socket)):
            yield fake, socket


def run(case, tmp_path):
    from codex_thread_bridge.rpc import AppServer

    async def drive():
        async with fake_host(case.get("given", {}), tmp_path) as (fake, socket):
            client = AppServer(socket)
            try:
                await client.connect()
                results = []
                for step in case["run"].get("steps", []):
                    results.append(await client.call(step["method"], step.get("params", {})))
            finally:
                await client.close()
        return {"exit": 0, "results": results, "calls": [[method, params] for method, params in fake.calls]}

    return asyncio.run(drive())
