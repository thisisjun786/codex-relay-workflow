"""Bridge-owned contract scenarios against the checkout Python program."""

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT))
from contract import runner as CORPUS  # noqa: E402

DOMAINS = ("mcp-tools", "appserver", "git", "ledger-fingerprint")


def pytest_generate_tests(metafunc):
    if "scenario" in metafunc.fixturenames:
        cases = CORPUS.scenarios(DOMAINS)
        metafunc.parametrize("scenario", cases, ids=[path.stem for path in cases])


if CORPUS.scenarios(DOMAINS):
    def test_contract_scenario(scenario, tmp_path):
        CORPUS.run_scenario(scenario, tmp_path)


def test_contract_corpus_discovery():
    assert CORPUS.FIXTURES == ROOT / "contract" / "fixtures"


def test_contract_git_readiness_proof(tmp_path):
    # Proof introduced with the runner, not a source class-A case.
    CORPUS.run_scenario(
        CORPUS.FIXTURES / "git" / "test_worktree__test_contract_git_readiness_proof.json",
        tmp_path,
    )


def test_appserver_project_read_proof(tmp_path):
    CORPUS.run_scenario(
        CORPUS.FIXTURES / "appserver" / "test_worktree__test_appserver_project_read_proof.json",
        tmp_path,
    )
