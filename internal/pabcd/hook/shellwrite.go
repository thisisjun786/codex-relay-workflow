package hook

type shellToken struct {
	token []uint16
	next  int
}

func ShellWriteDestinations(string) []string   { return []string{} }
func stripHeredocBodies([]uint16) []uint16     { return nil }
func heredocDelimiter([]uint16, int) []uint16  { return nil }
func splitShellSegments([]uint16) [][]uint16   { return nil }
func skipQuoted([]uint16, int) int             { return 0 }
func skipHeredoc([]uint16, int) int            { return 0 }
func readToken([]uint16, int) shellToken       { return shellToken{} }
func redirectDestinations([]uint16) [][]uint16 { return nil }
func tokenizeUnits([]uint16) [][]uint16        { return nil }
func tokenize(string) []string                 { return nil }
