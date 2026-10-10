package tomledit

// Statement is one statement of a document as the scanner located it: a [table] or [[array]] header, or a key = value line.
// Comments and blank lines are not statements; they lie between the spans.
type Statement struct {
	// Table and ArrayTable mark a header; both false is a key = value statement.
	Table, ArrayTable bool
	// Path is the full key path: the path a header names, or the section's path followed by the key as written.
	Path []string
	// Start is the offset of the statement's first line; End is past its line ending, or the end of the input.
	Start, End int
	// Multiline marks a value that spans lines or is an array or inline table.
	Multiline bool
}

// Statements answers the statements of a document, in order, with the strings and arrays of a value read whole so a
// header-like line inside one is not a statement. It is run on a document the decoder accepts, and answers an error for a
// form it cannot locate.
func Statements(content string) ([]Statement, error) {
	d, err := scan(content)
	if err != nil {
		return nil, err
	}
	out := make([]Statement, len(d.stmts))
	for i, st := range d.stmts {
		out[i] = Statement{Table: st.kind == tableStmt, ArrayTable: st.kind == arrayTableStmt, Path: st.path, Start: st.lineStart, End: st.end, Multiline: st.multiline}
	}
	return out, nil
}
