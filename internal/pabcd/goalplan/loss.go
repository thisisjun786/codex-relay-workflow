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
// judges, the text it was decoded from, which revivalLossDuplicate scans, and badByte, 1 plus the offset of the first byte that is
// not valid UTF-8 (0 when there is none). parsed is the zero value whenever the read returned no plan; text and badByte are also
// set for a file that parsed but did not revive, so the lock can tell stored data a write would lose from an absent plan.
// openErr is the failure of the walk to the plan file (an absence, an access failure, a link or a wrong kind of file), and refuse
// the reason when the text holds something a write would lose (an unpaired surrogate escape) and no plan could be built.
type revivalLossFile struct {
	parsed  any
	text    string
	badByte int
	openErr error
	refuse  string
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

// revivalLossDuplicate is the path of the first key that an object of text states twice, "" when none does. Decoding keeps the last
// value of a repeated key, so the earlier value, a whole review round list included, never reaches revivalLoss and a write would
// erase it. text is valid JSON: the read decoded it already.
func revivalLossDuplicate(text string) string {
	dec := json.NewDecoder(strings.NewReader(text))
	var walk func(path string) string
	walk = func(path string) string {
		tok, err := dec.Token()
		switch {
		case err != nil:
			return ""
		case tok == json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return ""
				}
				key, at := k.(string), revivalLossKey(path, k.(string))
				if seen[key] {
					return at
				}
				seen[key] = true
				if lost := walk(at); lost != "" {
					return lost
				}
			}
			_, _ = dec.Token()
		case tok == json.Delim('['):
			for i := 0; dec.More(); i++ {
				if lost := walk(fmt.Sprintf("%s[%d]", path, i)); lost != "" {
					return lost
				}
			}
			_, _ = dec.Token()
		}
		return ""
	}
	return walk("")
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
		if enc != nil {
			return here
		}
	case map[string]any:
		e, ok := enc.(map[string]any)
		if !ok {
			return here
		}
		for _, k := range slices.Sorted(maps.Keys(f)) {
			if f[k] == nil || path == "" && k == "updatedAt" {
				continue
			}
			// A key the encoding lacks reads as nil, which no non-null file value is covered by.
			if lost := revivalLossCovers(f[k], e[k], revivalLossKey(path, k)); lost != "" {
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
	a, b = revivalLossTrim(a), revivalLossTrim(b)
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

// revivalLossTrim is n in lower case without the zeros that end the fraction of its mantissa (1.500 is 1.5, 1.0 is 1): the value is
// the same, and a very long run of them would put an equal number outside what the big package accepts.
func revivalLossTrim(n json.Number) json.Number {
	mantissa, exponent, hasExponent := strings.Cut(strings.ToLower(string(n)), "e")
	if strings.Contains(mantissa, ".") {
		mantissa = strings.TrimSuffix(strings.TrimRight(mantissa, "0"), ".")
	}
	if hasExponent {
		return json.Number(mantissa + "e" + exponent)
	}
	return json.Number(mantissa)
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
