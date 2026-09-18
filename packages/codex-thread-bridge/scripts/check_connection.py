"""Read-only MCP smoke check against the existing App Server. Starts no model turns."""

import argparse
import asyncio
import json
import sys
import tempfile

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client


async def check(args):
    with tempfile.TemporaryDirectory(prefix="codex-thread-bridge-check-") as state:
        command = ["-m", "codex_thread_bridge.server", "--state-dir", state]
        if args.socket:
            command += ["--socket", args.socket]
        params = StdioServerParameters(command=sys.executable, args=command)
        async with stdio_client(params) as (read, write), ClientSession(read, write) as session:
            await session.initialize()
            tools = await session.list_tools()
            result = await session.call_tool("get_capabilities", {})
            if result.isError:
                raise RuntimeError(result.content)
            report = {
                "tools": [tool.name for tool in tools.tools],
                "connection": result.structuredContent,
            }
            if args.thread_id:
                history = await session.call_tool(
                    "read_thread",
                    {
                        "thread_id": args.thread_id,
                        "limit": 1,
                    },
                )
                if history.isError:
                    raise RuntimeError(history.content)
                # The page is nullable on purpose: read_thread returns the thread and says the
                # page was not observed rather than failing, and the thread most worth pointing
                # this script at is exactly the one that provokes that. Reporting the observation
                # is the diagnostic; crashing on it is not.
                page = history.structuredContent.get("turnsPage")
                report["readCheck"] = {
                    "status": history.structuredContent["thread"]["status"],
                    "turnCount": None if page is None else len(page["data"]),
                    "observation": history.structuredContent.get("observation"),
                }
                goal = await session.call_tool("get_goal", {"thread_id": args.thread_id})
                if goal.isError:
                    raise RuntimeError(goal.content)
                report["goalReadCheck"] = "passed"
            print(json.dumps(report, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket")
    parser.add_argument("--thread-id", help="Optionally verify reading one existing thread")
    asyncio.run(check(parser.parse_args()))
