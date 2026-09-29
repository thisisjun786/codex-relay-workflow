"""The plugin wiring declared before todo 34, for the tests that drive it.

The package now declares the native Stop command and `sh ./wiring/crw-bridge.sh`. A turn or
session that cached the older declaration still runs these until the cached Python commands
are retired (todo 43), and the tests of the Python launchers and the Python adapter drive them
here. The declarations are kept once, byte for byte, as the Go tests' testdata: a CI checkout
has no history to read them from.
"""

import json
from pathlib import Path

PRE_NATIVE = (Path(__file__).resolve().parents[3] / "internal" / "pluginwiring" / "testdata"
              / "pre-native-wiring")

# What the package declared before the native wiring, byte for byte.
LEGACY_MCP_DECLARATION = (PRE_NATIVE / "mcp.json").read_text(encoding="utf-8")
LEGACY_STOP_COMMAND = json.loads(
    (PRE_NATIVE / "hooks" / "stop-recording-completion.json").read_text(encoding="utf-8")
)["hooks"]["Stop"][0]["hooks"][0]["command"]
