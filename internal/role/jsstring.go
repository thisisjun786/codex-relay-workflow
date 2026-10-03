package role

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

const errToPrimitive = sentinel("Cannot convert object to primitive value")

// jsString is String(value) of a parsed JSON value, which the oracle's messages print for a value of the wrong type; no raw is
// undefined (an absent member). An array joins its elements with a comma and an empty text for null, an object is "[object Object]"
// unless it has an own member toString, which no JSON value can make callable: its ToPrimitive then throws, and so does String().
func jsString(raw json.RawMessage) (string, error) {
	if raw == nil {
		return "undefined", nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	return jsText(v)
}

func jsText(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "null", nil
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case json.Number:
		f, _ := strconv.ParseFloat(string(v), 64) // out of range is an infinity, as it is in JavaScript
		switch {
		case f > 1.7976931348623157e308:
			return "Infinity", nil
		case f < -1.7976931348623157e308:
			return "-Infinity", nil
		case f == 0:
			return "0", nil // -0 prints as 0
		}
		b, err := json.Marshal(f) // ECMAScript's spelling of a number: 1e+21, 1e-7
		return string(b), err
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			if e == nil {
				continue
			}
			var err error
			if parts[i], err = jsText(e); err != nil {
				return "", err
			}
		}
		return strings.Join(parts, ","), nil
	case map[string]any:
		if _, ok := v["toString"]; ok {
			return "", errToPrimitive
		}
	}
	return "[object Object]", nil
}
