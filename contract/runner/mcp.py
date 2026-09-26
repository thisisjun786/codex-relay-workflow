"""MCP stdio calls to the real bridge server and installed plugin launcher."""

import hashlib
import json
import os
import shutil
import subprocess
import sys

import anyio

from .cli import resolve
from .core import ROOT


def run(case, tmp_path):
    from mcp import ClientSession, StdioServerParameters
    from mcp.client.stdio import stdio_client

    async def drive():
        from .appserver import fake_host

        given = case.get("given", {})
        for name, contents in given.get("files", {}).items():
            path = tmp_path / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(resolve(contents, {}, tmp_path), encoding="utf-8")

        host_config = dict(given.get("host", {}))
        cursor_format = host_config.pop("cursor_format", None)
        host_config = resolve(host_config, {}, tmp_path)
        async with fake_host({"host": host_config}, tmp_path) as (fake, socket):
            if cursor_format == "json_turns":
                fake.cursor = lambda offset: json.dumps({"offset": offset, "scope": {"kind": "turns"}})
                fake.offset = lambda cursor: 0 if cursor is None else json.loads(cursor)["offset"]

            socket_path = socket
            if case["run"].get("socket_alias"):
                socket_path = socket.parent / "alias.sock"
                socket_path.symlink_to(socket)

            env = {**os.environ, "PYTHONPATH": str(ROOT / "packages/codex-thread-bridge/src"),
                   **resolve(case["run"].get("env", {}), {}, tmp_path)}
            args = ["-m", "codex_thread_bridge.server", "--socket", str(socket_path),
                    "--state-dir", str(tmp_path / "state")]
            command = sys.executable
            cwd = None
            if case["run"].get("launcher"):
                package = tmp_path / "codex-home/plugins/cache/crw/crw/0.0.0"
                (package / "wiring").mkdir(parents=True)
                shutil.copyfile(ROOT / "plugins/crw/wiring/crw_bridge_mcp.py",
                                package / "wiring/crw_bridge_mcp.py")
                home = tmp_path / "codex-home"
                (home / "config.toml").write_text("", encoding="utf-8")
                record = {"recordVersion": 1, "owner": "plugin",
                          "serverName": "codex-thread-bridge", "bridgeExecutable": sys.executable,
                          "args": args}
                if "policy" in given:
                    policy_path = tmp_path / "execution-policy.json"
                    data = json.dumps(resolve(given["policy"], {}, tmp_path)).encode()
                    policy_path.write_bytes(data)
                    record["recordVersion"] = 2
                    record["executionPolicy"] = {"path": str(policy_path),
                                                  "digest": hashlib.sha256(data).hexdigest()}
                (home / "crw-bridge-mcp.json").write_text(json.dumps(record), encoding="utf-8")
                user_home = tmp_path / "user-home"
                user_home.mkdir()
                env = {"HOME": str(user_home), "PATH": os.environ.get("PATH", ""),
                       "PYTHONPATH": str(ROOT / "packages/codex-thread-bridge/src"),
                       "CODEX_HOME": "", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY": "",
                       "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST": ""}
                args = ["./wiring/crw_bridge_mcp.py"]
                cwd = str(package)

            if case["run"].get("process"):
                done = await anyio.to_thread.run_sync(
                    lambda: subprocess.run([command, *args], env=env, cwd=cwd, input="",
                                           text=True, capture_output=True, check=False, timeout=60))
                return {"exit": done.returncode, "stderr": done.stderr,
                        "state_exists": (tmp_path / "state").exists(),
                        "calls": [[method, params] for method, params in fake.calls]}

            results = []
            names = {}
            tools = {}
            sessions = case["run"].get("sessions", [case["run"].get("steps", [])])
            for index, steps in enumerate(sessions):
                if index and case["run"].get("socket_alias"):
                    args[args.index(str(socket_path))] = str(socket)
                parameters = StdioServerParameters(command=command, args=args, env=env, cwd=cwd)
                async with stdio_client(parameters) as (read, write), ClientSession(read, write) as session:
                    await session.initialize()
                    listed = await session.list_tools()
                    tools = {tool.name: tool.inputSchema for tool in listed.tools}
                    for step in steps:
                        if "host_set" in step:
                            target = fake
                            keys = step["host_set"]["path"]
                            for key in keys[:-1]:
                                target = target[key] if isinstance(target, dict) else getattr(target, key)
                            value = resolve(step["host_set"]["value"], names, tmp_path)
                            if isinstance(target, dict):
                                target[keys[-1]] = value
                            else:
                                setattr(target, keys[-1], value)
                            continue
                        result = await session.call_tool(
                            step["tool"], resolve(step.get("arguments", {}), names, tmp_path))
                        observed = {"error": bool(result.isError),
                                    "content": [part.model_dump(mode="json") for part in result.content],
                                    "structured": result.structuredContent}
                        results.append(observed)
                        names[step.get("id", str(len(results) - 1))] = observed
            return {"exit": 0, "tools": tools, "results": results,
                    "calls": [[method, params] for method, params in fake.calls
                              if method in {"thread/start", "thread/resume", "turn/start",
                                            "thread/turns/list", "thread/list", "turn/steer"}]}

    return anyio.run(drive)
