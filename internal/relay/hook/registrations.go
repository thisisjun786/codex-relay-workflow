package hook

// AdapterCommand is one registration of this adapter in the user hook file: its identity, its
// command line and that line's words as the POSIX quoting subset splits them.
type AdapterCommand struct {
	Identity, Command string
	Words             []string
}

// AdapterCommands is AdapterIdentities with each registration's command and words, so a caller
// can tell what a registration runs: the installer refuses to move the owned pointer out from
// under an interpreter or script a registration reaches through it. readable is false when the
// file could not be read, which is never an empty file.
func AdapterCommands(path, event string) ([]AdapterCommand, bool) {
	_, ours, readable := readRegistrations(path, event)
	out := make([]AdapterCommand, 0, len(ours))
	for _, registration := range ours {
		words, _ := shellSplit(nativeExitSuffix.ReplaceAllString(registration.Command, ""))
		out = append(out, AdapterCommand{Identity: registration.Identity, Command: registration.Command, Words: words})
	}
	return out, readable
}
