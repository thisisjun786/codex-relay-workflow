package goalplan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"unicode/utf8"
)

// revivalLossFile is what the write lock needs of a read besides the plan: the JSON value the file decoded to, which revivalLoss
// judges, and badByte, 1 plus the offset of the first byte that is not valid UTF-8 (0 when there is none). Both are the zero value
// whenever the read returned no plan.
type revivalLossFile struct {
	parsed  any
	badByte int
}

// revivalLossBadByte is revivalLossFile.badByte of raw. Revival decodes such bytes to U+FFFD, which would then read as equal to the
// U+FFFD of the re-encoding and be written in their place.
func revivalLossBadByte(raw []byte) int {
	if utf8.Valid(raw) {
		return 0
	}
	for i := 0; ; {
		r, n := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && n == 1 {
			return i + 1
		}
		i += n
	}
}

// revivalLossRefusal is the reason the write lock gives for a plan it will not hand to a writer.
func revivalLossRefusal(slug, what string) string {
	return fmt.Sprintf("goalplan '%s' holds %s that this build cannot keep; refusing to rewrite it", slug, what)
}

// revivalLoss is the first place of a stored plan that a write of its revived form would not keep, as a JSON path, or "" when
// nothing would be lost. The plan the file revives to is encoded as the writer encodes it and decoded back, and the file must be
// covered by that re-encoding (revivalLossCovers). That sees every drop and coercion of revival at once: a skipped task, a dropped
// criteriaIds entry, a key no reviver reads, a sub-record a reviver discarded, a value it replaced. A plan that revival refuses has
// nothing to write and gives "". The argument must be the decode readPlanAt used, whose refusals of unpaired surrogate escapes and,
// for the lock, of bytes that are not UTF-8 keep a string from reaching the comparison after a lossy decode.
func revivalLoss(parsed any) string {
	plan := reviveGoalplan(parsed, nil)
	if plan == nil {
		return ""
	}
	data, err := writeEncodePlan(*plan)
	if err != nil {
		return "(plan)"
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var enc any
	if dec.Decode(&enc) != nil {
		return "(plan)"
	}
	return revivalLossCovers(parsed, enc, "")
}

// revivalLossCovers is the path of the first place where file is not covered by enc, "" when it is. An object is covered when each
// of its keys, visited in sorted order because decoding loses the order of the file, is present in enc with a covering value; a
// null may be absent or replaced, and so may the top-level updatedAt, which the writer refreshes. An array is covered by an array
// of the same length element by element; the first place the two diverge is reported, so after a dropped element the positions
// behind it are not compared. Strings and booleans must be equal, numbers exactly equal.
func revivalLossCovers(file, enc any, path string) string {
	here := path
	if here == "" {
		here = "(plan)"
	}
	switch f := file.(type) {
	case nil:
	case map[string]any:
		e, ok := enc.(map[string]any)
		if !ok {
			return here
		}
		for _, k := range slices.Sorted(maps.Keys(f)) {
			if f[k] == nil || path == "" && k == "updatedAt" {
				continue
			}
			v, present := e[k]
			if !present {
				return revivalLossKey(path, k)
			}
			if lost := revivalLossCovers(f[k], v, revivalLossKey(path, k)); lost != "" {
				return lost
			}
		}
	case []any:
		e, ok := enc.([]any)
		if !ok {
			return here
		}
		for i := range min(len(f), len(e)) {
			if lost := revivalLossCovers(f[i], e[i], fmt.Sprintf("%s[%d]", path, i)); lost != "" {
				return lost
			}
		}
		if len(f) > len(e) {
			return fmt.Sprintf("%s[%d]", path, len(e))
		} else if len(f) < len(e) {
			return here
		}
	case json.Number:
		if e, ok := enc.(json.Number); !ok || !revivalLossNumber(f, e) {
			return here
		}
	default:
		if file != enc {
			return here
		}
	}
	return ""
}

// revivalLossNumber is exact equality of two JSON numbers (9007199254740993 is not 9007199254740992, 1e21 is 1e+21). A zero equals
// only a zero, whatever its exponent; for any other value a text the big package refuses, an exponent beyond its limit, equals
// only the identical text.
func revivalLossNumber(a, b json.Number) bool {
	if zeroA, zeroB := revivalLossZero(a), revivalLossZero(b); zeroA || zeroB {
		return zeroA && zeroB
	}
	x, okA := new(big.Rat).SetString(string(a))
	y, okB := new(big.Rat).SetString(string(b))
	if !okA || !okB {
		return a == b
	}
	return x.Cmp(y) == 0
}

// revivalLossZero is whether the mantissa of n, the text before an exponent, holds no digit 1 to 9.
func revivalLossZero(n json.Number) bool {
	mantissa, _, _ := strings.Cut(strings.ToLower(string(n)), "e")
	return !strings.ContainsAny(mantissa, "123456789")
}

// revivalLossKey is key under path: ".key" for a plain identifier, a bracketed JSON string otherwise.
func revivalLossKey(path, key string) string {
	plain := key != ""
	for i := 0; i < len(key) && plain; i++ {
		c := key[i]
		plain = c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9'
	}
	switch {
	case !plain:
		return path + "[" + quote(key) + "]"
	case path == "":
		return key
	}
	return path + "." + key
}
