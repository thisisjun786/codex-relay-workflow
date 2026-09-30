// Package hook implements the fail-open Stop adapter and its read-only guard.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type Object = contract.OrderedObject
type Field = contract.Field

func get(o Object, key string) any { return evidence.Get(o, key) }
func object(v any) Object          { o, _ := evidence.Object(v); return o }
func text(v any) string            { s, _ := v.(string); return s }
func set(o Object, key string, v any) Object {
	for i := range o {
		if o[i].Key == key {
			o[i].Value = v
			return o
		}
	}
	return append(o, Field{Key: key, Value: v})
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Decode retains Python's object order and integer/float distinction without panic paths.
func Decode(raw []byte) (any, error) {
	if _, err := store.DecodeUTF8(raw); err != nil {
		return nil, err
	}
	if message := store.PythonJSONError(string(raw)); message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	return decodeScanned(raw)
}

// decodeScanned is Decode past its checks: raw is UTF-8 that Python's JSON scanner accepts.
func decodeScanned(raw []byte) (any, error) {
	raw, constants := jsonConstants(raw)
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	v, err := decodeValue(d, raw, constants)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return v, nil
}

// decodeToken keeps Python str code points (including lone surrogates in WTF-8)
// using the bridge ledger's existing decoder. encoding/json still owns structure
// and offsets, but its replacement-character string value must never reach hashes.
func decodeToken(d *json.Decoder, raw []byte) (any, error) {
	start := d.InputOffset()
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	if _, ok := token.(string); ok {
		spelling := raw[start:d.InputOffset()]
		at := strings.IndexByte(string(spelling), '"')
		if at < 0 {
			return nil, fmt.Errorf("missing string token")
		}
		return decodeString(spelling[at:])
	}
	return token, nil
}

// decodeString decodes a string token's spelling, quotes included. A lone surrogate the
// document holds as its WTF-8 bytes rather than as an escape (json.loads decoded the frame's
// bytes with surrogatepass, pyjson.DecodeBytesWTF8) is one character of the string, kept as it
// stands; the spans around it are decoded as JSON. Python's scanner pairs only two \u escapes,
// so such a surrogate pairs with nothing, and it cannot sit inside an escape the scan accepted.
func decodeString(spelling []byte) (any, error) {
	body := spelling[1 : len(spelling)-1]
	var out strings.Builder
	from := 0
	for i := 0; i+3 <= len(body); i++ {
		if body[i] != 0xed || body[i+1] < 0xa0 || body[i+1] > 0xbf || body[i+2] < 0x80 || body[i+2] > 0xbf {
			continue
		}
		part, err := ledger.DecodeJSON([]byte("\"" + string(body[from:i]) + "\""))
		if err != nil {
			return nil, err
		}
		out.WriteString(part.(string))
		out.Write(body[i : i+3])
		from = i + 3
		i += 2
	}
	if from == 0 {
		return ledger.DecodeJSON(spelling)
	}
	part, err := ledger.DecodeJSON([]byte("\"" + string(body[from:]) + "\""))
	if err != nil {
		return nil, err
	}
	out.WriteString(part.(string))
	return out.String(), nil
}

func decodeValue(d *json.Decoder, raw []byte, constants map[int64]float64) (any, error) {
	tok, err := decodeToken(d, raw)
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		if v == '[' {
			a := []any{}
			for d.More() {
				item, e := decodeValue(d, raw, constants)
				if e != nil {
					return nil, e
				}
				a = append(a, item)
			}
			_, err = d.Token()
			return a, err
		}
		o := Object{}
		for d.More() {
			key, e := decodeToken(d, raw)
			if e != nil {
				return nil, e
			}
			item, e := decodeValue(d, raw, constants)
			if e != nil {
				return nil, e
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			o = set(o, name, item)
		}
		_, err = d.Token()
		return o, err
	case json.Number:
		if number, ok := constants[d.InputOffset()]; ok {
			return number, nil
		}
		if strings.ContainsAny(string(v), ".eE") {
			n, err := v.Float64()
			if math.IsInf(n, 0) {
				return n, nil
			} // Python json.loads accepts exponent overflow.
			return n, err
		}
		if n, e := v.Int64(); e == nil {
			return n, nil
		}
	}
	return tok, nil
}
func decodeObject(raw []byte) (Object, error) {
	v, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	o, ok := evidence.Object(v)
	if !ok {
		return nil, fmt.Errorf("the Stop payload must be a JSON object")
	}
	return o, nil
}
