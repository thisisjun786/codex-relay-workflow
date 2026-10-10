// Package tomledit is the semantic boundary for the few keys CRW edits in a TOML file it shares with another owner (Codex's
// config.toml). It answers what a key is from the whole document decoded by BurntSushi/toml, the decoder the rest of CRW reads
// the same file with, and it edits one key with the smallest byte change: only the bytes of that key's value, of that key's
// line, or one appended line or table are touched, so comments, line endings and every other setting stay as they were. An
// edit is checked before it is returned: the candidate must decode, the key must hold the value asked for, and with that key
// taken out the candidate must decode to exactly what the original did. Anything it cannot place or check is refused, never
// guessed, and nothing here opens a file.
package tomledit

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// State is what a lookup found.
type State int

// The four answers of Get. Absent is a key the document does not define at all; Found is a key with a single-line value this
// package can rewrite; Unsupported is a key (or its table) the document defines in a form this package will not edit, such as
// an inline table, an array of tables or a multi-line value; Invalid is a document that does not decode.
const (
	Absent State = iota
	Found
	Unsupported
	Invalid
)

func (s State) String() string {
	switch s {
	case Absent:
		return "absent"
	case Found:
		return "found"
	case Unsupported:
		return "unsupported"
	case Invalid:
		return "invalid"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// Lookup is the answer of Get.
type Lookup struct {
	State State
	// Raw is the value as written, without its comment tail or surrounding blanks (Found only).
	Raw string
	// Value is the decoded value (Found, and Unsupported when the key itself is defined).
	Value any
	// Defined reports whether the decoded document holds the key, whatever its form.
	Defined bool
	// Reason says why the answer is Unsupported or Invalid.
	Reason string
}

// Refusal is the error of an edit this package will not make. The document is to be left as it is.
type Refusal struct {
	State  State
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

// IsRefusal reports whether err is a Refusal, and returns it.
func IsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	ok := errors.As(err, &r)
	return r, ok
}

// Edit is the outcome of an edit.
type Edit struct {
	// Content is the new document, identical to the input when nothing changed.
	Content string
	// Prior is the raw value found before the edit, nil when the key was absent.
	Prior   *string
	Changed bool
}

// Decode decodes a whole document the way every other CRW reader of config.toml does.
func Decode(content string) (map[string]any, error) {
	m := map[string]any{}
	if _, err := toml.Decode(content, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Validate reports whether content is a TOML document.
func Validate(content string) error {
	_, err := Decode(content)
	return err
}

// DecodeValue decodes one raw value as it would be read on the right of "k = ".
func DecodeValue(raw string) (any, error) {
	m, err := Decode("k = " + raw + "\n")
	if err != nil {
		return nil, err
	}
	return m["k"], nil
}

// SameValue reports whether two raw values decode to the same value. A raw value that does not decode is equal to nothing.
func SameValue(a, b string) bool {
	va, ea := DecodeValue(a)
	vb, eb := DecodeValue(b)
	return ea == nil && eb == nil && Equal(va, vb)
}

// semantic walks the decoded document to path. It answers whether the key is defined, its value, and a reason when a
// component on the way is not a plain table (an array of tables, or a value that is not a table).
func semantic(doc map[string]any, path []string) (value any, defined bool, reason string) {
	cur := doc
	for i, name := range path {
		v, ok := cur[name]
		if !ok {
			return nil, false, ""
		}
		if i == len(path)-1 {
			return v, true, ""
		}
		switch t := v.(type) {
		case map[string]any:
			cur = t
		case []map[string]any:
			return nil, false, fmt.Sprintf("%s is an array of tables", strings.Join(path[:i+1], "."))
		default:
			return nil, false, fmt.Sprintf("%s is a value, not a table", strings.Join(path[:i+1], "."))
		}
	}
	return nil, false, ""
}

// Get answers what the document says about path (a table path and the key as its last element).
func Get(content string, path []string) Lookup {
	if len(path) == 0 {
		return Lookup{State: Unsupported, Reason: "an empty key path"}
	}
	doc, err := Decode(content)
	if err != nil {
		return Lookup{State: Invalid, Reason: "config.toml is not valid TOML: " + err.Error()}
	}
	value, defined, reason := semantic(doc, path)
	if reason != "" {
		return Lookup{State: Unsupported, Reason: reason}
	}
	doc2, err := scan(content)
	if err != nil {
		return Lookup{State: Unsupported, Value: value, Defined: defined, Reason: err.Error()}
	}
	matches := doc2.keysAt(path)
	if !defined {
		if len(matches) > 0 {
			return Lookup{State: Unsupported, Reason: "the key is written where the decoder does not read it"}
		}
		return Lookup{State: Absent}
	}
	if _, isTable := value.(map[string]any); isTable {
		return Lookup{State: Unsupported, Value: value, Defined: true, Reason: strings.Join(path, ".") + " is a table, not a value"}
	}
	if len(matches) != 1 {
		return Lookup{State: Unsupported, Value: value, Defined: true, Reason: strings.Join(path, ".") + " is defined inside an inline table or array crw does not edit"}
	}
	st := doc2.stmts[matches[0]]
	if st.multiline {
		return Lookup{State: Unsupported, Value: value, Defined: true, Reason: strings.Join(path, ".") + " holds a multi-line string, an array or an inline table, which crw does not rewrite"}
	}
	return Lookup{State: Found, Raw: content[st.valStart:st.valEnd], Value: value, Defined: true}
}

// Set writes path = raw, where raw is a TOML value (for a boolean, "true" or "false"). An existing single-line value is
// replaced in place; an absent key is added to its table (a [table] section, or next to the dotted keys that define the table),
// or a new table is appended. Refused: an invalid document, a key in a form Get calls Unsupported, and any candidate that fails
// the check described in the package comment.
func Set(content string, path []string, raw string) (Edit, error) {
	want, err := DecodeValue(raw)
	if err != nil {
		return Edit{}, &Refusal{Unsupported, "the new value is not a TOML value: " + err.Error()}
	}
	look := Get(content, path)
	switch look.State {
	case Invalid, Unsupported:
		return Edit{}, &Refusal{look.State, look.Reason}
	case Found:
		prior := look.Raw
		if prior == raw {
			return Edit{Content: content, Prior: &prior}, nil
		}
		doc, _ := scan(content)
		st := doc.stmts[doc.keysAt(path)[0]]
		cand := content[:st.valStart] + raw + content[st.valEnd:]
		if err := check(content, cand, path, want, true); err != nil {
			return Edit{}, err
		}
		return Edit{Content: cand, Prior: &prior, Changed: true}, nil
	}
	doc, err := scan(content)
	if err != nil {
		return Edit{}, &Refusal{Unsupported, err.Error()}
	}
	cand := doc.insert(content, path, raw)
	if err := check(content, cand, path, want, true); err != nil {
		return Edit{}, err
	}
	return Edit{Content: cand, Changed: true}, nil
}

// Restore puts path back to prior: the raw value it had, or no key at all when prior is nil. An absent key is left absent (a
// restore never adds a key the document does not have); a value form Get calls Unsupported is refused.
func Restore(content string, path []string, prior *string) (Edit, error) {
	look := Get(content, path)
	switch look.State {
	case Invalid, Unsupported:
		return Edit{}, &Refusal{look.State, look.Reason}
	case Absent:
		return Edit{Content: content}, nil
	}
	found := look.Raw
	doc, _ := scan(content)
	st := doc.stmts[doc.keysAt(path)[0]]
	if prior == nil {
		cand := content[:st.lineStart] + content[st.end:]
		if err := check(content, cand, path, nil, false); err != nil {
			return Edit{}, err
		}
		return Edit{Content: cand, Prior: &found, Changed: true}, nil
	}
	if found == *prior {
		return Edit{Content: content, Prior: &found}, nil
	}
	want, err := DecodeValue(*prior)
	if err != nil {
		return Edit{}, &Refusal{Unsupported, "the recorded value is not a TOML value: " + err.Error()}
	}
	cand := content[:st.valStart] + *prior + content[st.valEnd:]
	if err := check(content, cand, path, want, true); err != nil {
		return Edit{}, err
	}
	return Edit{Content: cand, Prior: &found, Changed: true}, nil
}

// check is the gate every candidate passes: it decodes, path holds want (or is absent when present is false), and nothing
// else differs from the original once path is taken out of both.
func check(pre, cand string, path []string, want any, present bool) error {
	before, err := Decode(pre)
	if err != nil {
		return &Refusal{Invalid, "config.toml is not valid TOML: " + err.Error()}
	}
	after, err := Decode(cand)
	if err != nil {
		return &Refusal{Unsupported, "the edit would leave config.toml invalid (" + err.Error() + "); nothing was written"}
	}
	got, defined, _ := semantic(after, path)
	if defined != present || present && !Equal(got, want) {
		return &Refusal{Unsupported, "the edit would not set " + strings.Join(path, ".") + " as asked; nothing was written"}
	}
	strip(before, path)
	strip(after, path)
	if !Equal(before, after) {
		return &Refusal{Unsupported, "the edit would change more than " + strings.Join(path, ".") + "; nothing was written"}
	}
	return nil
}

// strip removes path from a decoded document, then every table on the way that the removal left empty, so a table created
// only to hold the key compares equal to its absence.
func strip(doc map[string]any, path []string) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		delete(doc, path[0])
		return
	}
	child, ok := doc[path[0]].(map[string]any)
	if !ok {
		return
	}
	strip(child, path[1:])
	if len(child) == 0 {
		delete(doc, path[0])
	}
}

// Equal compares two decoded values. NaN equals NaN, and two times are equal when they are the same instant in the same
// zone, so a document compares equal to itself.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	case []map[string]any:
		y, ok := b.([]map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || math.IsNaN(x) && math.IsNaN(y))
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y) && x.Location().String() == y.Location().String()
	}
	return reflect.DeepEqual(a, b)
}
