package hook

import "slices"

// shellWriteDestsTest is the write destinations of a command as the memory gate reads them: the reader's records, with an
// unknown destination kept as the unknown mark. A command the reader cannot read gives the unknown mark alone.
func shellWriteDestsTest(command string) []string {
	d, ok := shellIRWriteDests(command, "/work", nil)
	if !ok {
		return []string{shellIRUnknownDest}
	}
	if d == nil {
		return []string{}
	}
	return d
}

// destsCover is whether the destinations the reader gives cover the wanted ones: every wanted path is reported, or the
// reader reports a destination it cannot evaluate, which the gate treats as a write needing a grant. Extra reports are
// allowed.
func destsCover(got, want []string) bool {
	if slices.Contains(got, shellIRUnknownDest) {
		return true
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			return false
		}
	}
	return true
}
