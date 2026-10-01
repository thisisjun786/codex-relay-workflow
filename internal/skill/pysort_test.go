package skill

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// TestPySortedMatchesLivePython sorts JSON lists with pySorted and holds the sorted lists, as
// json.dumps spells them (so 1, 1.0 and true stay distinct and a stable sort must keep them in
// Python's order), or the TypeError each raises, to the golden, first taken as what CPython's
// sorted() answered: line n of the golden is case n's answer. NaN makes the result depend on the
// exact sequence of comparisons, and the long structured lists reach the merge stack, merge_lo,
// merge_hi and galloping.
func TestPySortedMatchesLivePython(t *testing.T) {
	// Given lists of numbers with NaN, strings, lists, and unorderable mixtures.
	lines := pySortCases(rand.New(rand.NewPCG(42, 1024)))
	errors := 0
	var sortedLines strings.Builder
	for i, line := range lines {
		// When Go sorts the decoded values.
		decoded, err := pythonLoads([]byte(line))
		if err != nil {
			t.Fatalf("case %d does not decode: %v", i, err)
		}
		got := ""
		sorted, err := pySorted(orderedPlain(decoded).([]any))
		if err != nil {
			got = err.Error()
			errors++
		} else {
			got = pyjson.Dumps(sorted, pyjson.Options{})
		}
		sortedLines.WriteString(got + "\n")
	}
	// Then each order, or TypeError, is the golden's.
	checkSkillValue(t, "sorted", []byte(sortedLines.String()))
	if errors == 0 || errors == len(lines) {
		t.Fatalf("the corpus should both sort and raise; %d of %d raised", errors, len(lines))
	}
	t.Logf("%d lists sorted as Python sorts them, %d raising Python's TypeError", len(lines)-errors, errors)
}

// pySortCases spells each case as one JSON line, keeping NaN, 1.0 and 1e400 literal.
func pySortCases(r *rand.Rand) []string {
	numbers := []string{"NaN", "0", "1", "1.0", "true", "false", "0.0", "-0.0", "2", "2.5", "-3", "7",
		"12345678901234567890", "1e19", "1e400", "-1e400", "9007199254740993", "9007199254740992.0"}
	strs := []string{`"a"`, `"b"`, `"ab"`, `""`, `"B"`, `"é"`, `"한"`, `"😀"`, `"z"`, `"a\u0000"`}
	intruders := []string{`"x"`, `null`, `{}`, `{"a": 1}`, `[1]`, `["a"]`, `1`, `NaN`, `true`}
	size := func() int {
		switch r.IntN(4) {
		case 0:
			return r.IntN(8)
		case 1:
			return r.IntN(70)
		case 2:
			return 60 + r.IntN(300)
		default:
			return 500 + r.IntN(2500)
		}
	}
	// structured returns n values with long runs, so merges gallop.
	structured := func(n int, value func(int) string) []string {
		out := make([]string, 0, n)
		for len(out) < n {
			run := 1 + r.IntN(200)
			start := r.IntN(400) - 100
			step := []int{1, -1, 0, 2}[r.IntN(4)]
			for i := 0; i < run && len(out) < n; i++ {
				out = append(out, value(start+i*step))
			}
		}
		return out
	}
	join := func(items []string) string { return "[" + strings.Join(items, ", ") + "]" }
	var cases []string
	for range 700 {
		n := size()
		var items []string
		switch r.IntN(9) {
		case 0, 1: // numbers of every kind, NaN included
			for range n {
				items = append(items, numbers[r.IntN(len(numbers))])
			}
		case 2: // long runs of ints with NaN, floats and bools sprinkled in
			items = structured(n, func(i int) string { return fmt.Sprint(i) })
			for i := range items {
				switch r.IntN(40) {
				case 0:
					items[i] = "NaN"
				case 1:
					items[i] = items[i] + ".0"
				case 2:
					items[i] = "true"
				}
			}
		case 3: // strings
			for range n {
				items = append(items, strs[r.IntN(len(strs))]+"")
			}
			for i := range items {
				if r.IntN(3) == 0 {
					items[i] = fmt.Sprintf(`"%c%d"`, 'a'+r.IntN(3), r.IntN(50))
				}
			}
		case 4: // lists compared item by item, NaN inside
			for range n {
				inner := make([]string, r.IntN(3))
				for j := range inner {
					inner[j] = []string{"1", "2", "NaN", "1.0", "true"}[r.IntN(5)]
				}
				items = append(items, join(inner))
			}
		case 5: // lists of strings
			for range n {
				inner := make([]string, r.IntN(3))
				for j := range inner {
					inner[j] = strs[r.IntN(4)]
				}
				items = append(items, join(inner))
			}
		case 7: // unorderable pairs that first meet inside a merge
			if r.IntN(2) == 0 {
				// Ascending runs of [key, tail], longer than minrun, one tail type per run:
				// '<' reaches an int tail and a str tail only when two runs' keys tie.
				for range 2 + r.IntN(12) {
					base, tail := r.IntN(400), `"t"`
					if r.IntN(2) == 0 {
						tail = "0"
					}
					for key := range 64 + r.IntN(150) {
						items = append(items, fmt.Sprintf("[%d, %s]", base+key*(1+r.IntN(3)), tail))
					}
				}
				break
			}
			half, stringsFirst := 40+r.IntN(200), r.IntN(2) == 0
			for i := range 2 * half {
				if (i < half) == stringsFirst {
					items = append(items, fmt.Sprintf(`"s%d"`, r.IntN(100)))
				} else {
					items = append(items, fmt.Sprint(r.IntN(100)))
				}
			}
		default: // an orderable list with one or two values '<' refuses
			items = structured(max(n, 2), func(i int) string { return fmt.Sprint(i) })
			if r.IntN(2) == 0 {
				for i := range items {
					items[i] = fmt.Sprintf(`"s%05d"`, i*7%1000)
				}
			}
			for range 1 + r.IntN(2) {
				items[r.IntN(len(items))] = intruders[r.IntN(len(intruders))]
			}
		}
		cases = append(cases, join(items))
	}
	return cases
}
