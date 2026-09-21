"""The managed adapter's ledger follows the explicit store, not the environment.

The RPC is a stand-in. The ledger is the pinned bridge ledger opened by the real adapter,
so the file that appears is the one a later retry would reuse.
"""

import os
import shutil
import tempfile
import unittest
from pathlib import Path


class ManagedAdapterLedger(unittest.TestCase):
    def setUp(self):
        try:
            import codex_thread_bridge  # noqa: F401
        except ImportError:
            self.skipTest("the pinned bridge is not importable in this interpreter")
        root = os.environ.get("TMPDIR") or tempfile.gettempdir()
        self.tmp = tempfile.mkdtemp(prefix="managed-ledger-", dir=root)
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.state = Path(self.tmp) / "explicit-state"
        self.other = Path(self.tmp) / "env-state"
        self.state.mkdir()
        self.other.mkdir()
        self.socket = Path(self.tmp) / "app.sock"
        self._previous = os.environ.get("CODEX_SESSION_RELAY_STATE")
        os.environ["CODEX_SESSION_RELAY_STATE"] = str(self.other)
        self.addCleanup(self._restore_env)

    def _restore_env(self):
        if self._previous is None:
            os.environ.pop("CODEX_SESSION_RELAY_STATE", None)
        else:
            os.environ["CODEX_SESSION_RELAY_STATE"] = self._previous

    def _server(self):
        class Idle:
            socket_path = None
            info = {}

            async def call(self, method, params):
                raise AssertionError(f"unexpected {method}")

            async def close(self):
                return None

        return Idle()

    def _adapter(self, *, pin):
        from codex_session_relay.bridge_adapter import BridgeHostAdapter
        from codex_thread_bridge.execution import PRESENCE_ONLY

        options = {"execution_policy": PRESENCE_ONLY, "app_server_factory": lambda _canonical: self._server()}
        if pin:
            options["ledger_directory"] = self.state
        adapter = BridgeHostAdapter(str(self.socket), timeout=5, **options)
        self.addCleanup(adapter.close)
        return adapter

    def _files(self, directory):
        return sorted(path.name for path in directory.glob("operations-*.sqlite3"))

    def test_an_unpinned_adapter_follows_the_environment(self):
        adapter = self._adapter(pin=False)
        identity = adapter.ledger_identity_record()
        self.assertTrue(str(identity["realPath"]).startswith(str(self.other.resolve())))
        self.assertEqual(self._files(self.state), [])
        self.assertEqual(len(self._files(self.other)), 1)

    def test_a_pinned_adapter_ignores_a_different_environment_directory(self):
        first = self._adapter(pin=True)
        opened = first.ledger_identity_record()
        self.assertTrue(str(opened["realPath"]).startswith(str(self.state.resolve())))
        self.assertIsNotNone(opened["device"])
        self.assertIsNotNone(opened["inode"])
        self.assertEqual(self._files(self.other), [])
        first.close()

        os.environ["CODEX_SESSION_RELAY_STATE"] = str(Path(self.tmp) / "retry-state")
        second = self._adapter(pin=True)
        again = second.ledger_identity_record()
        self.assertEqual((again["device"], again["inode"]), (opened["device"], opened["inode"]))
        self.assertEqual(again["realPath"], opened["realPath"])
        self.assertEqual(len(self._files(self.state)), 1)
        self.assertFalse((Path(self.tmp) / "retry-state").exists())

    def test_a_replaced_ledger_file_is_refused_before_a_mutation(self):
        adapter = self._adapter(pin=True)
        opened = adapter.ledger_identity_record()
        target = Path(opened["realPath"])
        # A new inode at the same path is a different ledger, even when the bytes match.
        replacement_bytes = target.read_bytes()
        target.unlink()
        target.write_bytes(replacement_bytes)
        from codex_session_relay.hostadapter import HostUnavailable

        with self.assertRaises(HostUnavailable):
            adapter.require_ledger(opened)

    def test_require_ledger_accepts_the_identity_it_just_reported(self):
        adapter = self._adapter(pin=True)
        opened = adapter.ledger_identity_record()
        again = adapter.require_ledger(opened)
        self.assertEqual((again["device"], again["inode"]), (opened["device"], opened["inode"]))
        self.assertEqual(again["realPath"], opened["realPath"])


if __name__ == "__main__":
    unittest.main()
