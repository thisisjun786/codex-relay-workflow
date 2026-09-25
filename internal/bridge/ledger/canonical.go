package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Fingerprint matches Python json.dumps([method, params], sort_keys=True,
// separators=(",", ":")) followed by SHA-256. Decode with UseNumber to retain
// integer spelling across the JSON boundary.
func Fingerprint(method string, params map[string]any) (string, error) {
	var b bytes.Buffer
	if err := canonical(&b, []any{method, params}); err != nil {
		return "", err
	}
	hash := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(hash[:]), nil
}

func canonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, x)
	case json.Number:
		n := string(x)
		if strings.ContainsAny(n, ".eE") {
			f, err := x.Float64()
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return fmt.Errorf("float %q: %w", n, err)
			}
			return floatJSON(b, f)
		}
		if n == "-0" {
			n = "0"
		}
		b.WriteString(n)
	case float64:
		return floatJSON(b, x)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteByte('{')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// Python sorts by Unicode code points, matching UTF-8 lexicographic order.
		slices.Sort(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonical(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := canonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", v)
	}
	return nil
}

func floatJSON(b *bytes.Buffer, f float64) error {
	if math.IsNaN(f) {
		b.WriteString("NaN")
		return nil
	}
	if math.IsInf(f, 1) {
		b.WriteString("Infinity")
		return nil
	}
	if math.IsInf(f, -1) {
		b.WriteString("-Infinity")
		return nil
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		exponent, err := strconv.Atoi(s[i+1:])
		if err != nil {
			return fmt.Errorf("exponent: %w", err)
		}
		if exponent >= -4 && exponent < 16 {
			s = strconv.FormatFloat(f, 'f', -1, 64)
			if !strings.Contains(s, ".") {
				s += ".0"
			}
		} else {
			s = s[:i+1] + fmt.Sprintf("%+03d", exponent)
		}
	} else if !strings.Contains(s, ".") {
		s += ".0"
	}
	b.WriteString(s)
	return nil
}

// writeString is Python's ensure_ascii string encoder. A lone surrogate stored by DecodeJSON
// is written back as its own \uXXXX escape, which is what json.dumps emits for "\ud800".
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		if r, ok := surrogateAt(s, i); ok {
			fmt.Fprintf(b, `\u%04x`, r)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0xffff {
				fmt.Fprintf(b, `\u%04x`, r)
			} else if r >= 0x10000 {
				r -= 0x10000
				fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+r>>10, 0xdc00+r&0x3ff)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
