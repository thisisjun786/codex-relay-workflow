package shellir

import "testing"

// TestProgramCreatedInADirectoryByTheText: cp, mv, install and ln write a file inside a directory operand (the last operand, or the
// -t / --target-directory one) named like the source. A program word that names such a file is what the text wrote
// (CRW-1028 verifier finding 4).
func TestProgramCreatedInADirectoryByTheText(t *testing.T) {
	for _, cmd := range []string{
		"cp -t bin evil/tool; bin/tool",
		"cp --target-directory=bin evil/tool; bin/tool",
		"cp --target-directory bin evil/tool; ./bin/tool",
		"cp -tbin evil/tool; bin/tool",
		"cp -at bin evil/tool; bin/tool",
		"cp evil/tool bin; bin/tool",
		"cp evil/tool bin/; bin/tool",
		"cp evil/tool ./bin; ./bin/tool",
		"mv evil/tool bin; bin/tool",
		"mv -t bin evil/tool; bin/tool",
		"install -m 755 evil/tool bin; bin/tool",
		"install -t bin evil/tool; bin/tool",
		"ln -s /bin/bash bin; bin/bash",
		"ln -st bin /bin/bash; bin/bash",
		"ln -s /bin/bash; ./bash",
		"cp -r evil bin; bin/evil/tool",
		"cp -r evil bin; bin/evil/sub/tool",
		"cd sub; cp -t bin ../evil/tool; bin/tool",
		"cp -t bin a b; bin/b",
		"cp -- evil/tool bin; bin/tool",
		"cp evil/tool bin; ./bin/../bin/tool",
	} {
		analyzeUnreadable(t, cmd, true)
	}
	for _, cmd := range []string{
		"cp -t bin evil/tool; bin/other",
		"cp evil/tool bin; other/tool",
		"cp evil/tool bin; ./tool",
		"cp a.txt .; ./bin/tool",
		"cp -T evil/tool bin; ./other",
		"cp evil/tool bin",
		"ln -s /bin/bash bin; ./other",
		"cp -S .bak a b; ./c",
	} {
		analyzeUnreadable(t, cmd, false)
	}
}
