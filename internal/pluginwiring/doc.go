// Package pluginwiring is the Go side of the commands the CRW plugin package declares
// (plugins/crw/wiring, decision 26): `codex-thread-bridge --plugin-launch`, which
// wiring/crw-bridge.sh execs through the runtime pointer, reads and judges the bridge record the
// way crw_bridge_mcp.py did and then execs this binary as the bridge. Its tests run the declared
// commands the way the host runs them: the Stop hook through `sh -c`, the MCP server as
// `sh ./wiring/crw-bridge.sh` from the package root.
package pluginwiring
