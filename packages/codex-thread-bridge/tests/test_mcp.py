import asyncio
import hashlib
import json
import os
import shutil
import sys
from pathlib import Path

import pytest
from mcp import StdioServerParameters

APPROVED = {"model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
POLICY = {"allowed": [{"model": APPROVED["model"], "efforts": [APPROVED["reasoning_effort"]]}]}


def server_parameters(socket, state, environment=None):
    return StdioServerParameters(
        command=sys.executable,
        args=[
            "-m",
            "codex_thread_bridge.server",
            "--socket",
            str(socket),
            "--state-dir",
            str(state),
        ],
        env={**os.environ, **(environment or {})},
    )


async def test_the_schema_itself_refuses_a_mutation_that_states_no_pair(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_the_schema_itself_refuses_a_mutation_that_states_no_pair.json",
                            tmp_path)


async def test_an_argument_a_caller_invents_grants_it_nothing(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_an_argument_a_caller_invents_grants_it_nothing.json",
                            tmp_path)


async def test_an_unconfigured_host_accepts_a_stated_pair_and_says_so(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_an_unconfigured_host_accepts_a_stated_pair_and_says_so.json",
                            tmp_path)


async def test_a_configured_host_admits_the_approved_pair_and_refuses_the_rest(
    fake_server, tmp_path
):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_a_configured_host_admits_the_approved_pair_and_refuses_the_rest.json",
                            tmp_path)


async def test_a_configured_policy_that_cannot_be_used_stops_the_server(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_a_configured_policy_that_cannot_be_used_stops_the_server.json",
                            tmp_path)


# Started the way Codex Desktop starts it
#
# Codex Desktop spawns this server through the crw plugin's launcher, with the App Server's bare
# environment: no CODEX_HOME and no execution-policy variable. These start it the same way: the
# repository's own launcher, copied to where an installation puts it under a temporary Codex home
# and started from the package root the declaration names, so it has to find its record from its
# own location. HOME is somewhere else, so a launcher that fell back to the user's home would find
# no record rather than a real one. The launcher is outside this package, so a run of this suite
# without the repository around it skips here -- and scripts/ci/packages.py fails any skipped
# case, so the repository's own run never skips.

LAUNCHER = Path(__file__).resolve().parents[3] / "plugins" / "crw" / "wiring" / "crw_bridge_mcp.py"
CHILD = {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}
ROLE_POLICY = {"roles": {"child": CHILD,
                         "parent": {"model": "devin/swe-2", "reasoningEffort": "max"}}}


def launched_by_the_plugin(tmp_path, socket, *, policy=ROLE_POLICY):
    if not LAUNCHER.is_file():
        pytest.skip("the crw plugin launcher is not beside this package: " + str(LAUNCHER))
    home = tmp_path / "codex-home"
    package = home / "plugins" / "cache" / "crw" / "crw" / "0.0.0"
    (package / "wiring").mkdir(parents=True)
    shutil.copyfile(LAUNCHER, package / "wiring" / LAUNCHER.name)
    # What makes the directory six levels up a Codex home to the launcher, rather than only a
    # directory that happens to be there.
    (home / "config.toml").write_text("", encoding="utf-8")
    record = {"recordVersion": 1, "owner": "plugin", "serverName": "codex-thread-bridge",
              "bridgeExecutable": sys.executable,
              "args": ["-m", "codex_thread_bridge.server", "--socket", str(socket),
                       "--state-dir", str(tmp_path / "state")]}
    digest = None
    if policy is not None:
        path = tmp_path / "execution-policy.json"
        data = json.dumps(policy).encode()
        path.write_bytes(data)
        digest = hashlib.sha256(data).hexdigest()
        record.update(recordVersion=2, executionPolicy={"path": str(path), "digest": digest})
    (home / "crw-bridge-mcp.json").write_text(json.dumps(record))
    # Only what the host hands a plugin server: the SDK's own short list (HOME, PATH and the
    # like), which carries neither CODEX_HOME nor an execution-policy variable, with HOME moved.
    user_home = tmp_path / "user-home"
    user_home.mkdir()
    parameters = StdioServerParameters(command=sys.executable, args=["./wiring/" + LAUNCHER.name],
                                       cwd=str(package), env={"HOME": str(user_home)})
    return parameters, digest


async def test_the_plugin_launched_bridge_reports_the_host_policy_and_checks_the_role(
    fake_server, tmp_path
):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_the_plugin_launched_bridge_reports_the_host_policy_and_checks_the_role.json",
                            tmp_path)


async def test_a_plugin_record_naming_no_policy_starts_exactly_as_before(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_a_plugin_record_naming_no_policy_starts_exactly_as_before.json",
                            tmp_path)


async def test_a_server_started_expecting_another_digest_never_starts(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_a_server_started_expecting_another_digest_never_starts.json",
                            tmp_path)


async def test_mcp_forwards_json_shaped_pagination_cursors(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_mcp_forwards_json_shaped_pagination_cursors.json",
                            tmp_path)


async def test_real_mcp_stdio_discovery_create_read_and_dedup(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_real_mcp_stdio_discovery_create_read_and_dedup.json",
                            tmp_path)


async def test_mcp_socket_alias_restart_does_not_repeat_creation(fake_server, tmp_path):
    from contract.runner import FIXTURES, run_scenario
    await asyncio.to_thread(run_scenario, FIXTURES / "mcp-tools" /
                            "test_mcp__test_mcp_socket_alias_restart_does_not_repeat_creation.json",
                            tmp_path)
