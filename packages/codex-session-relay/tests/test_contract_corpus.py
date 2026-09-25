"""Relay-owned contract scenarios against the checkout Python program."""

from pathlib import Path

import pytest

import sys

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT))
from contract import runner as CORPUS
DOMAINS = ("cli-shape", "exit-codes", "sqlite-ddl", "hook", "records", "event-key")


@pytest.mark.parametrize("scenario", CORPUS.scenarios(DOMAINS), ids=lambda path: path.stem)
def test_contract_scenario(scenario, tmp_path):
    CORPUS.run_scenario(scenario, tmp_path)


def test_contract_corpus_discovery():
    assert CORPUS.FIXTURES == ROOT / "contract" / "fixtures"


def test_cli_service_uses_isolated_scope(tmp_path):
    # Proof introduced with the runner, not a source class-A case.
    CORPUS.run_scenario(
        CORPUS.FIXTURES / "cli-shape" / "test_dispositions__test_cli_service_uses_isolated_scope.json",
        tmp_path,
    )
