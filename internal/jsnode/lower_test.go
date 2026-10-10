package jsnode

import (
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A lone surrogate reaches a Go string as the three WTF-8 bytes pyjson keeps for it, or as a byte outside UTF-8 (a path or argv). The
// oracle's toLowerCase leaves a lone surrogate as it is, so the original bytes stay, not a replacement character.
func TestLowerIsToLowerCase(t *testing.T) {
	wtf8 := "\xed\xa0\x80" // U+D800 as WTF-8, the spelling pyjson.CodePoint reads back
	for _, c := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"ascii", "ABC", "abc"},
		{"ci", "CI", "ci"},
		{"u0130", "\u0130", "i\u0307"},
		{"u0130 word", "\u0130STANBUL", "i\u0307stanbul"},
		{"final sigma", "\u039f\u03a3", "\u03bf\u03c2"},
		{"final sigma ending a word", "\u0391\u03a3 \u03a3", "\u03b1\u03c2 \u03c3"},
		{"non-final sigma", "\u03a3\u0391", "\u03c3\u03b1"},
		{"sigma after a case-ignorable", "A.\u03a3", "a.\u03c2"},
		{"sigma before a cased letter", "A\u03a3B", "a\u03c3b"},
		{"kelvin sign", "\u212a", "k"},
		{"accent", "\u00c9a", "\u00e9a"},
		{"titlecase", "\u01c5", "\u01c6"},
		{"hangul", "\ubc30\ud3ec", "\ubc30\ud3ec"},
		{"lone surrogate alone", wtf8, wtf8},
		{"lone surrogate between ascii", "x" + wtf8 + "Y", "x" + wtf8 + "y"},
		{"two lone surrogates", wtf8 + wtf8, wtf8 + wtf8},
		{"byte outside utf-8", "A\xffB", "a\xffb"},
		{"truncated sequence", "\xedAB", "\xedab"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Lower(c.in); got != c.want {
				t.Errorf("Lower(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// lowerByUnits is the earlier, array-based Lower: every code point is decoded into a slice before any is mapped, and the Final_Sigma
// rule reads that slice. Lower must agree with it on every input, since it only avoids the slices.
func lowerByUnits(s string) string {
	type unit struct {
		r         rune
		off, size int
	}
	var units []unit
	var rs []rune
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		units, rs = append(units, unit{r, i, n}), append(rs, r)
		i += n
	}
	var b strings.Builder
	for i, u := range units {
		switch {
		case pyjson.IsSurrogate(u.r):
			b.WriteString(s[u.off : u.off+u.size])
		case u.r == 0x130:
			b.WriteString("i̇")
		case u.r == 0x3a3 && finalSigmaByUnits(rs, i):
			b.WriteRune(0x3c2)
		default:
			b.WriteRune(unicode.ToLower(u.r))
		}
	}
	return b.String()
}

func finalSigmaByUnits(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && caseIgnorable(rs[j]) {
		j--
	}
	if j < 0 || !cased(rs[j]) {
		return false
	}
	k := i + 1
	for k < len(rs) && caseIgnorable(rs[k]) {
		k++
	}
	return k == len(rs) || !cased(rs[k])
}

func TestLowerAgreesWithTheArrayReading(t *testing.T) {
	pieces := []string{"A", "a", "z", "Z", "1", " ", ".", "'", ":", "^", "Σ", "Σ", "σ", "Α", "́", "·", "İ", "K",
		"ǅ", "É", "’", "\u200d", "ʰ", "\U0001F600", "배", "\xed\xa0\x80", "\xed\xbf\xbf", "\xff", "\xed", "\xe2\x80", "\xf0\x9f", "\x80"}
	rng := rand.New(rand.NewSource(451))
	for n := 0; n < 200000; n++ {
		var b strings.Builder
		for k := rng.Intn(9); k >= 0; k-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		in := b.String()
		if got, want := Lower(in), lowerByUnits(in); got != want {
			t.Fatalf("Lower(%q) = %q, the array reading gives %q", in, got, want)
		}
	}
}

// A search reads a whole file through Lower, so its allocation stays near the size of the text, not a table of units per byte.
func TestLowerOfALargeTextAllocatesLittle(t *testing.T) {
	text := strings.Repeat("Hello, World ΣΑ İ ", 40000) // about 1 MiB
	allocs := testing.AllocsPerRun(3, func() { _ = Lower(text) })
	if allocs > 4 {
		t.Errorf("Lower of %d bytes made %v allocations, want a handful", len(text), allocs)
	}
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	_ = Lower(text)
	runtime.ReadMemStats(&m1)
	if used := m1.TotalAlloc - m0.TotalAlloc; used > uint64(3*len(text)) {
		t.Errorf("Lower of %d bytes allocated %d bytes, want under three times the text", len(text), used)
	}
}

func BenchmarkLowerLarge(b *testing.B) {
	for name, text := range map[string]string{
		"ascii":   strings.Repeat("Hello, World. The Quick Brown Fox 0123456789\n", 24000),
		"unicode": strings.Repeat("Hello, World ΣΑ İ 배포\n", 30000),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Lower(text)
			}
		})
	}
}
