package shellir

import "testing"

// TestProgramCreatedInADirectoryByTheText: cp, mv, install and ln write a file inside a directory operand (the last operand, or the
// -t / --target-directory one) named like the source; a source that names a directory's contents (evil/.) fills the directory
// operand itself. A program word that names such a file is what the text wrote
// (CRW-1028 verifier finding 4; round 3 finding 1).
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
		// the destination that a directory source becomes itself (verifier round 2): every file below it is the source's
		"cp -rT evil bin; bin/tool",
		"cp -r evil bin; bin/tool",
		"cp -R evil bin/; bin/tool",
		"cp -a evil bin; bin/sub/tool",
		"cp --recursive evil bin; bin/tool",
		"mv evil bin; bin/tool",
		"mv -T evil bin; bin/tool",
		"ln -s evildir bin; bin/tool",
		"ln -sT evildir bin; bin/tool",
		"cp --parents evil/tool bin; bin/evil/tool",
		"cp --parents -t bin evil/tool; bin/evil/tool",
		"cd sub; cp -rT ../evil bin; bin/tool",
		// >&FILE writes the file (verifier round 2)
		"cat evil.sh >&tool.sh; ./tool.sh",
		"echo x 1>&tool.sh; ./tool.sh",
		// a source that names a directory's contents (dir/.) fills the target directory itself (verifier round 3)
		"cp -r -t bin evil/.; bin/tool",
		"cp -r --target-directory=bin evil/.; bin/tool",
		"cp -r --target-directory bin evil/./; bin/sub/tool",
		"cp -rt bin evil/.; ./bin/tool",
		"cp -r evil/. bin; bin/tool",
		"cp -a evil/. bin/; bin/tool",
		"cp -t bin evil/.; bin/tool",
		"cp -r -t bin evil/..; bin/tool",
		"cp -r -t bin .; bin/tool",
		"cp -r . bin; bin/tool",
		"cd sub; cp -r -t bin ../evil/.; bin/tool",
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
		"cp -rT evil bin; other/tool",
		"cp -r evil bin; ./tool",
		"cp evil/tool bin; bin/other", // no -r: a file is copied, the destination is not a tree
		"install -D evil/tool bin; bin/other",
		"echo x >&2; ./tool",
		"echo x >&2-; ./tool",
		"echo x 2>&1; ./tool",
		"echo x >&-; ./tool",
		"cat evil.sh >&other.sh; ./tool.sh",
		"cp -r -t bin evil/.; other/tool",
		"cp -r --target-directory=bin evil/.; ./tool",
		"cp -r evil/. bin; other/tool",
	} {
		analyzeUnreadable(t, cmd, false)
	}
}
