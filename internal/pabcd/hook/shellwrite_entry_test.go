package hook

// The two readers below are the python-default entries of shellVerbScriptWritesIn and shellVerbOpenWritesIn.
// The product reaches the readers through ShellWriteDestinations and shellVerbScriptWritesIn only, so the entries
// live here, with the unit tests of the readers that call them (CRW-818).

// shellVerbScriptWrites is scriptWriteDestinations (:535): the path of open(path, "w"), Path(path).write_text(...) and
// writeFile(path...) calls, pattern by pattern in that order. The oracle's backreferences (the closing quote repeats the opening
// one) become one alternative per quote; each alternation picks its branch by the quote present, so it never competes with
// lazy matching and each pattern matches what the backtracking original does. hard adds template literal paths, the open() and
// Path reader and the decoded value of a JavaScript literal, after the oracle's raw text.
func shellVerbScriptWrites(script string, hard bool) []string {
	return shellVerbScriptWritesIn(script, hard, true)
}

// shellVerbOpenWrites reads each open(...) call of a program as Python does, in one pass over the text: the path is the first
// argument or file=, the mode the second or mode=, in either order and with other keywords between. A call whose mode writes,
// appends, creates or updates (a w, a, x or + in it) names its path; only string literals count. One frame is kept per open
// bracket and arguments are spans of the text, so unclosed and nested calls cost no more than their own characters. A
// Path(...).write_text or .write_bytes call names the join of its arguments (shellWriteEscapePath).
func shellVerbOpenWrites(script string) []string {
	return shellVerbOpenWritesIn(script, true)
}
