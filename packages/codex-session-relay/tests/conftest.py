"""Fixtures every test of this package runs under."""

import pytest
from codex_session_relay.service import production_scope_root

from .support import production_registry_untouched

# Resolved once, at collection, before any test patches the passwd lookup or the function.
PRODUCTION_SCOPE_ROOT = production_scope_root()


@pytest.fixture(autouse=True)
def production_scope_registry_untouched():
    """A test that leaves a new entry in the real production scope registry fails.

    Reading its listing, before and after each test, is all this does there.
    """
    with production_registry_untouched(PRODUCTION_SCOPE_ROOT):
        yield
