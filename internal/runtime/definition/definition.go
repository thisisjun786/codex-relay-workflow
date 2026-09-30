// Package definition is the one compatibility definition (OPS-1.1): the two components, the
// console-script names the installer places beside crw as links to it, each component's version
// and licence, and the tool that identifies the bridge.
//
// Until todo 44 scripts/crw_runtime/components.json was that definition, read by the Python
// installer and the developer harnesses, and this package carried the fields a Go install uses
// and a test kept them equal to the file. The file left with its last Python reader, and this
// package is the only copy (decision 47). The upstream provenance it recorded for the ported
// bridge is packages/codex-thread-bridge/PROVENANCE.md. Nothing here carries a per-target binary digest: release digests live in the
// release's SHA256SUMS and in the host record (decision 35).
package definition

// Version is the definitionVersion a host record states (decision 34: the host record stays at
// definitionVersion 1). It is not the version of a file: no file carries the definition since
// todo 44.
const Version = 1

// Component is one component of the definition.
type Component struct {
	Name          string
	ConsoleScript string
	Version       string
	LicencePath   string
	IdentityTool  string
}

// Names of the two components.
const (
	Bridge = "codex-thread-bridge"
	Relay  = "codex-session-relay"
)

// Components is the definition, in the order components.json listed them.
var Components = []Component{
	{
		Name: Bridge, ConsoleScript: "codex-thread-bridge", Version: "0.2.0",
		LicencePath: "packages/codex-thread-bridge/LICENSE", IdentityTool: "get_capabilities",
	},
	{
		Name: Relay, ConsoleScript: "codex-session-relay", Version: "0.2.0",
		LicencePath: "LICENSE",
	},
}

// HookScript is the name the completion hook's entry point went by, a third link beside crw
// until decision 66; `crw hook` is the hook now, and the doctor still names the hook by it.
const HookScript = "crw-completion-hook"

// Links are the names the installer places beside crw, each a symlink to it.
func Links() []string {
	return []string{Components[1].ConsoleScript, Components[0].ConsoleScript}
}

// Of is the component with this name.
func Of(name string) (Component, bool) {
	for _, c := range Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}
