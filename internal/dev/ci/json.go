//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// JSON values as the checks read them: encoding/json's any, with numbers kept as json.Number so
// an integer and a fraction stay apart.

var errNotUTF8 = errors.New("not UTF-8 text")

// decodeJSON is the one JSON document data holds: UTF-8 text with nothing after the value.
func decodeJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errNotUTF8
	}
	// Unmarshal checks the whole text first, so a syntax error or trailing text is refused in
	// encoding/json's own words before the decoder reads the value.
	if err := json.Unmarshal(data, new(json.RawMessage)); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

// readText is a file's text, which must be UTF-8.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s: %w", path, errNotUTF8)
	}
	return string(data), nil
}

// object is the JSON object value holds, if it holds one.
func object(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok
}

// has reports whether the object m has the member key.
func has(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// show is a value as a message names it: its compact JSON text.
func show(value any) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// text is a string value itself and any other value as show spells it.
func text(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return show(value)
}

// integer is the integer a JSON number spells without a fraction or exponent.
func integer(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(string(number), 10, 64)
	return n, err == nil
}

// filled reports a value that is not null, false, zero or empty.
func filled(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0
	}
	return true
}

// same reports equal JSON values.
func same(a, b any) bool { return reflect.DeepEqual(a, b) }
