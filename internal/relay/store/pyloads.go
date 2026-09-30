package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// LoadsJSON is json.loads into values contract.Emit renders as Python would: objects keep
// their key order (a repeated key keeps its first position and last value), integers stay
// exact as json.Number, NaN and the infinities are float64, and every other number becomes a
// float64. A document json.loads refuses answers its JSONDecodeError text (PythonJSONError).
func LoadsJSON(data []byte) (any, error) {
	if message := PythonJSONError(string(data)); message != "" {
		return nil, errors.New(message)
	}
	data, constants := jsonConstants(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := loadsValue(decoder, constants)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		//lint:ignore ST1005 json/decoder.py:348 caller-visible message kept byte-identical to Python
		return nil, errors.New("Extra data")
	}
	return value, nil
}

// jsonConstants replaces only unquoted non-finite tokens. Offsets identify the
// numeric tokens, so strings and ordinary zero values cannot collide with them.
func jsonConstants(data []byte) ([]byte, map[int64]float64) {
	out := make([]byte, 0, len(data))
	constants := map[int64]float64{}
	quoted, escaped := false, false
	for i := 0; i < len(data); {
		c := data[i]
		if !quoted {
			matched := false
			for _, one := range []struct {
				text  string
				value float64
			}{{"NaN", math.NaN()}, {"Infinity", math.Inf(1)}, {"-Infinity", math.Inf(-1)}} {
				if bytes.HasPrefix(data[i:], []byte(one.text)) {
					out = append(out, '0')
					constants[int64(len(out))] = one.value
					i += len(one.text)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		out = append(out, c)
		i++
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
		}
	}
	return out, constants
}

func loadsValue(decoder *json.Decoder, constants map[int64]float64) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch v := token.(type) {
	case json.Delim:
		if v == '[' {
			array := []any{}
			for decoder.More() {
				item, err := loadsValue(decoder, constants)
				if err != nil {
					return nil, err
				}
				array = append(array, item)
			}
			_, err := decoder.Token()
			return array, err
		}
		object := contract.OrderedObject{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			item, err := loadsValue(decoder, constants)
			if err != nil {
				return nil, err
			}
			name, _ := key.(string)
			replaced := false
			for i := range object {
				if object[i].Key == name {
					object[i].Value, replaced = item, true
					break
				}
			}
			if !replaced {
				object = append(object, contract.Field{Key: name, Value: item})
			}
		}
		_, err := decoder.Token()
		return object, err
	case json.Number:
		if f, ok := constants[decoder.InputOffset()]; ok {
			return f, nil
		}
		if strings.ContainsAny(string(v), ".eE") {
			f, _ := strconv.ParseFloat(string(v), 64)
			return f, nil
		}
		return v, nil
	default:
		return token, nil
	}
}

// PythonStrip is str.strip(): Unicode White_Space plus the ASCII information separators
// U+001C..U+001F, which str.isspace counts and unicode.IsSpace does not.
func PythonStrip(value string) string {
	return strings.TrimFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// PathlibSpelling is str(Path(value)): empty and "." components collapse, ".." stays, and exactly
// two leading slashes stay a root of their own where three or more fold to one.
func PathlibSpelling(value string) string { return pathlibSpelling(value) }

// Absolute is str(Path(value).absolute()) for a path already expanded: the kernel's working
// directory (os.getcwd, never $PWD's spelling of it) prefixed to a relative path, then the
// pathlib spelling, nothing resolved and no ".." folded.
func Absolute(value string) (string, error) { return absolute(value) }

// Home is Path.home(): HOME when it is set at all (an empty HOME is the root, trailing slashes
// are dropped), else this user's passwd entry; ErrNoHome where neither answers.
func Home() (string, error) { return homeDir() }

// AbsoluteExpanded is str(Path(value).expanduser().absolute()): home expanded (ErrNoHome for an
// unknown ~user), the working directory prefixed to a relative path, nothing resolved and no
// ".." folded, so a symlink the caller named stays the path they named.
func AbsoluteExpanded(value string) (string, error) { return absoluteExpanded(value) }
