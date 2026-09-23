"""A relay store object that has shipped keeps the CREATE text it shipped with.

SQLite keeps every CREATE statement verbatim, comments inside a table body included, and
CREATE ... IF NOT EXISTS never rewrites an object a store already holds. The runtime swap gate
(scripts/crw_runtime/swapgate.py) compares the objects a store holds with a candidate's by that
text, whitespace collapsed. So a candidate that changes the text of a shipped object - even one
comment line inside a table - is refused on every store that already holds it, and nothing ever
converges it. New explanation goes in a comment line before the CREATE keyword, which SQLite does
not store, or in the documentation.

relay_schema_shipped.json is the schema a fresh Store creates at the revision it names (0ffcc4d0,
installed on the production host on 2026-09-23), read with the swap gate's own query. Objects a
later revision adds are not in it and are not judged here; when a later revision is installed,
its schema becomes the snapshot.
"""

import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))
sys.path.insert(0, str(ROOT / "packages" / "codex-session-relay" / "src"))

from crw_runtime import swapgate  # noqa: E402
from codex_session_relay.store import Store  # noqa: E402

SNAPSHOT = Path(__file__).with_name("relay_schema_shipped.json")


class ShippedRelaySchema(unittest.TestCase):
    def test_every_shipped_object_keeps_the_text_the_swap_gate_compares(self):
        shipped = json.loads(SNAPSHOT.read_text(encoding="utf-8"))
        with tempfile.TemporaryDirectory() as root:
            store = Store(str(Path(root) / "relay.sqlite3"))
            try:
                current = dict(store.db.execute(swapgate.SCHEMA_OBJECTS_QUERY).fetchall())
            finally:
                store.close()
        held = shipped["objects"]
        lost = sorted(set(held) - set(current))
        changed = sorted(name for name in set(held) & set(current)
                         if swapgate._normalised(held[name]) != swapgate._normalised(current[name]))
        self.assertEqual(
            ([], []), (lost, changed),
            f"objects shipped at {shipped['revision'][:8]} were dropped or now have different"
            " CREATE text; the swap gate refuses every store that holds them. Restore the text"
            " and put new explanation before the CREATE keyword.",
        )
        self.assertGreater(len(held), 50, "the snapshot stopped describing the shipped store")


if __name__ == "__main__":
    unittest.main()
