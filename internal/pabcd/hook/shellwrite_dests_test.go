package hook

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
