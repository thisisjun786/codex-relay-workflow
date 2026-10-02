package cli_test

// The command families the cli package does not link itself register in the relay command table
// here too, so the in-process tests see the commands the built crw has.
import (
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)
