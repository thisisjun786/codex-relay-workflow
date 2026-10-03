package store

import "testing"

// Text reads a TEXT column's string and a BLOB column's bytes as text, and nothing else.
func TestRowTextReadsTextAndBlobColumnsOnly(t *testing.T) {
	r := Row{{Name: "t", Value: "text"}, {Name: "b", Value: []byte("blob")}, {Name: "n", Value: nil}, {Name: "i", Value: int64(7)}, {Name: "f", Value: 1.5}}
	for name, want := range map[string]string{"t": "text", "b": "blob", "n": "", "i": "", "f": "", "absent": ""} {
		if got := r.Text(name); got != want {
			t.Errorf("Text(%q) = %q, want %q", name, got, want)
		}
	}
}
