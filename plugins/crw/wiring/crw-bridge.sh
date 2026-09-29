#!/bin/sh
# The task bridge the plugin declares: the runtime pointer's codex-thread-bridge (a link to crw).
exec "$HOME/.local/share/crw-runtime/current/bin/codex-thread-bridge" --plugin-launch "$@"
