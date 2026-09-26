// Package faults is the relay's operational fault ledger (faults.py, faultsweep.py).
//
// Subset ported for todo 21; todo 22 owns this package. Only what the delivery hold paths
// exercise is here: FaultLedger.record for the observations the sweep derives (identity,
// occurrences, the timeline, suppression, the state transition, the opening and escalation
// publications and the blocking notification), and the two sweep sources that read deliveries
// (delivery_faults, retry_faults) with the clears recovered() derives for their class. Every
// other source, adoption, workspaces, rescoping, reopening and the publication worker are todo
// 22's; a path that would need one of them is refused with an error naming it rather than
// answered differently from Python.
package faults

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// dumps is Python json.dumps(value, ensure_ascii=False, sort_keys=True); compact selects
// separators=(",", ":").
func dumps(value any, compact bool) string {
	var b strings.Builder
	write(&b, value, compact)
	return b.String()
}

func write(b *strings.Builder, value any, compact bool) {
	item, key := ", ", ": "
	if compact {
		item, key = ",", ":"
	}
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case string:
		writeString(b, v)
	case int:
		b.WriteString(strconv.Itoa(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case float64:
		b.WriteString(pyFloat(v))
	case json.Number:
		b.WriteString(v.String())
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(item)
			}
			writeString(b, k)
			b.WriteString(key)
			write(b, v[k], compact)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, x := range v {
			if i > 0 {
				b.WriteString(item)
			}
			write(b, x, compact)
		}
		b.WriteByte(']')
	case []string:
		list := make([]any, len(v))
		for i, s := range v {
			list[i] = s
		}
		write(b, list, compact)
	default:
		panic(fmt.Sprintf("faults: unsupported JSON value %T", value))
	}
}

func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			for _, u := range utf16.Encode([]rune{r}) {
				fmt.Fprintf(b, "\\u%04x", u)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// pyFloat is float.__repr__.
func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	m := math.Abs(f)
	if m == 0 || m >= 1e-4 && m < 1e16 {
		text := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	text := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exponent, _ := strings.Cut(text, "e")
	sign := exponent[0]
	digits := strings.TrimLeft(exponent[1:], "0")
	if len(digits) < 2 {
		digits = fmt.Sprintf("%02s", digits)
	}
	return mantissa + "e" + string(sign) + digits
}

// loads decodes JSON into map[string]any / []any / json.Number / string / bool / nil.
func loads(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func loadsMap(text string) map[string]any {
	v, err := loads(text)
	if err != nil {
		return map[string]any{}
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func named(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}
