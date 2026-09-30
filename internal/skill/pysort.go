package skill

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// pySorted is sorted(items) over decoded JSON values, as CPython 3.13 runs it.
//
// The order and operands of every comparison follow Objects/listobject.c
// (count_run, binarysort, the powersort merge stack, merge_lo/merge_hi with
// galloping), because they are observable twice over: an unorderable pair
// raises the TypeError of the first comparison the sort makes, naming the two
// operands in the order it passed them, and a NaN, which is neither less nor
// greater than anything, lands wherever this exact sequence of comparisons
// leaves it. Each comparison is Python's '<' (pyLess).
func pySorted(items []any) ([]any, error) {
	keys := make([]any, len(items))
	copy(keys, items)
	sorter := pySort{keys: keys, minGallop: pySortMinGallop}
	if err := sorter.run(); err != nil {
		return nil, err
	}
	return keys, nil
}

const (
	pySortMinGallop = 7
	pySortMaxMinrun = 64
)

type pySortRun struct{ base, length, power int }

type pySort struct {
	keys      []any
	minGallop int
	pending   []pySortRun
}

func (s *pySort) run() error {
	remaining := len(s.keys)
	if remaining < 2 {
		return nil
	}
	minrun := pySortMinrun(remaining)
	lo := 0
	for remaining > 0 {
		n, err := s.countRun(lo, remaining)
		if err != nil {
			return err
		}
		if n < minrun {
			force := min(remaining, minrun)
			if err := s.binarySort(lo, force, n); err != nil {
				return err
			}
			n = force
		}
		if err := s.foundNewRun(n); err != nil {
			return err
		}
		s.pending = append(s.pending, pySortRun{base: lo, length: n})
		lo += n
		remaining -= n
	}
	return s.forceCollapse()
}

func pySortMinrun(n int) int {
	r := 0
	for n >= pySortMaxMinrun {
		r |= n & 1
		n >>= 1
	}
	return n + r
}

// binarySort sorts keys[lo:lo+n] by binary insertion, keys[lo:lo+ok] being sorted.
func (s *pySort) binarySort(lo, n, ok int) error {
	a := s.keys[lo : lo+n]
	if ok == 0 {
		ok = 1
	}
	for ; ok < n; ok++ {
		left, right := 0, ok
		pivot := a[ok]
		for left < right {
			middle := (left + right) >> 1
			less, err := pyLess(pivot, a[middle])
			if err != nil {
				return err
			}
			if less {
				right = middle
			} else {
				left = middle + 1
			}
		}
		copy(a[left+1:ok+1], a[left:ok])
		a[left] = pivot
	}
	return nil
}

func pySortReverse(a []any) {
	for i, j := 0, len(a)-1; i < j; i, j = i+1, j-1 {
		a[i], a[j] = a[j], a[i]
	}
}

// countRun is count_run: the length of the run at keys[lo:], made ascending in
// place, where a descending run keeps its equal elements in their first order.
func (s *pySort) countRun(lo, remaining int) (int, error) {
	a := s.keys[lo : lo+remaining]
	n := 1
	for ; n < remaining; n++ {
		smaller, err := pyLess(a[n], a[n-1])
		if err != nil {
			return 0, err
		}
		if smaller {
			break
		}
	}
	if n == remaining {
		return n, nil
	}
	if n > 1 {
		less, err := pyLess(a[0], a[n-1])
		if err != nil {
			return 0, err
		}
		if less {
			return n, nil
		}
		pySortReverse(a[:n])
	}
	n++
	equal := 0
	reverseEqual := func() {
		if equal > 0 {
			equal++
			pySortReverse(a[n-equal : n])
			equal = 0
		}
	}
	for ; n < remaining; n++ {
		smaller, err := pyLess(a[n], a[n-1])
		if err != nil {
			return 0, err
		}
		if smaller {
			reverseEqual()
			continue
		}
		larger, err := pyLess(a[n-1], a[n])
		if err != nil {
			return 0, err
		}
		if larger {
			break
		}
		equal++
	}
	reverseEqual()
	pySortReverse(a[:n])
	for ; n < remaining; n++ {
		smaller, err := pyLess(a[n], a[n-1])
		if err != nil {
			return 0, err
		}
		if smaller {
			break
		}
	}
	return n, nil
}

// gallopLeft is gallop_left: the k in 0..n with a[k-1] < key <= a[k].
func gallopLeft(key any, a []any, hint int) (int, error) {
	n := len(a)
	lastofs, ofs := 0, 1
	less, err := pyLess(a[hint], key)
	if err != nil {
		return 0, err
	}
	if less {
		maxofs := n - hint
		for ofs < maxofs {
			less, err := pyLess(a[hint+ofs], key)
			if err != nil {
				return 0, err
			}
			if !less {
				break
			}
			lastofs = ofs
			ofs = ofs<<1 + 1
		}
		ofs = min(ofs, maxofs)
		lastofs += hint
		ofs += hint
	} else {
		maxofs := hint + 1
		for ofs < maxofs {
			less, err := pyLess(a[hint-ofs], key)
			if err != nil {
				return 0, err
			}
			if less {
				break
			}
			lastofs = ofs
			ofs = ofs<<1 + 1
		}
		ofs = min(ofs, maxofs)
		lastofs, ofs = hint-ofs, hint-lastofs
	}
	lastofs++
	for lastofs < ofs {
		middle := lastofs + (ofs-lastofs)>>1
		less, err := pyLess(a[middle], key)
		if err != nil {
			return 0, err
		}
		if less {
			lastofs = middle + 1
		} else {
			ofs = middle
		}
	}
	return ofs, nil
}

// gallopRight is gallop_right: the k in 0..n with a[k-1] <= key < a[k].
func gallopRight(key any, a []any, hint int) (int, error) {
	n := len(a)
	lastofs, ofs := 0, 1
	less, err := pyLess(key, a[hint])
	if err != nil {
		return 0, err
	}
	if less {
		maxofs := hint + 1
		for ofs < maxofs {
			less, err := pyLess(key, a[hint-ofs])
			if err != nil {
				return 0, err
			}
			if !less {
				break
			}
			lastofs = ofs
			ofs = ofs<<1 + 1
		}
		ofs = min(ofs, maxofs)
		lastofs, ofs = hint-ofs, hint-lastofs
	} else {
		maxofs := n - hint
		for ofs < maxofs {
			less, err := pyLess(key, a[hint+ofs])
			if err != nil {
				return 0, err
			}
			if less {
				break
			}
			lastofs = ofs
			ofs = ofs<<1 + 1
		}
		ofs = min(ofs, maxofs)
		lastofs += hint
		ofs += hint
	}
	lastofs++
	for lastofs < ofs {
		middle := lastofs + (ofs-lastofs)>>1
		less, err := pyLess(key, a[middle])
		if err != nil {
			return 0, err
		}
		if less {
			ofs = middle
		} else {
			lastofs = middle + 1
		}
	}
	return ofs, nil
}

// mergeLo is merge_lo: merges keys[pa:pa+na] with the run after it, na <= nb.
func (s *pySort) mergeLo(pa, na, pb, nb int) error {
	keys := s.keys
	tmp := append([]any(nil), keys[pa:pa+na]...)
	dest, ta := pa, 0
	keys[dest] = keys[pb]
	dest, pb, nb = dest+1, pb+1, nb-1
	var err error
	if nb == 0 {
		goto succeed
	}
	if na == 1 {
		goto copyB
	}
	for {
		acount, bcount := 0, 0
		for {
			less, e := pyLess(keys[pb], tmp[ta])
			if e != nil {
				err = e
				goto fail
			}
			if less {
				keys[dest] = keys[pb]
				dest, pb = dest+1, pb+1
				bcount, acount = bcount+1, 0
				nb--
				if nb == 0 {
					goto succeed
				}
				if bcount >= s.minGallop {
					break
				}
			} else {
				keys[dest] = tmp[ta]
				dest, ta = dest+1, ta+1
				acount, bcount = acount+1, 0
				na--
				if na == 1 {
					goto copyB
				}
				if acount >= s.minGallop {
					break
				}
			}
		}
		s.minGallop++
		for {
			if s.minGallop > 1 {
				s.minGallop--
			}
			k, e := gallopRight(keys[pb], tmp[ta:ta+na], 0)
			if e != nil {
				err = e
				goto fail
			}
			acount = k
			if k > 0 {
				copy(keys[dest:dest+k], tmp[ta:ta+k])
				dest, ta, na = dest+k, ta+k, na-k
				if na == 1 {
					goto copyB
				}
				if na == 0 {
					goto succeed
				}
			}
			keys[dest] = keys[pb]
			dest, pb, nb = dest+1, pb+1, nb-1
			if nb == 0 {
				goto succeed
			}
			k, e = gallopLeft(tmp[ta], keys[pb:pb+nb], 0)
			if e != nil {
				err = e
				goto fail
			}
			bcount = k
			if k > 0 {
				copy(keys[dest:dest+k], keys[pb:pb+k])
				dest, pb, nb = dest+k, pb+k, nb-k
				if nb == 0 {
					goto succeed
				}
			}
			keys[dest] = tmp[ta]
			dest, ta, na = dest+1, ta+1, na-1
			if na == 1 {
				goto copyB
			}
			if acount < pySortMinGallop && bcount < pySortMinGallop {
				break
			}
		}
		s.minGallop++
	}
succeed:
fail:
	if na > 0 {
		copy(keys[dest:dest+na], tmp[ta:ta+na])
	}
	return err
copyB:
	copy(keys[dest:dest+nb], keys[pb:pb+nb])
	keys[dest+nb] = tmp[ta]
	return nil
}

// mergeHi is merge_hi: merges keys[pa:pa+na] with the run after it, na >= nb.
func (s *pySort) mergeHi(pa, na, pb, nb int) error {
	keys := s.keys
	tmp := append([]any(nil), keys[pb:pb+nb]...)
	basea := pa
	dest := pb + nb - 1
	tb := nb - 1
	pa += na - 1
	keys[dest] = keys[pa]
	dest, pa, na = dest-1, pa-1, na-1
	var err error
	if na == 0 {
		goto succeed
	}
	if nb == 1 {
		goto copyA
	}
	for {
		acount, bcount := 0, 0
		for {
			less, e := pyLess(tmp[tb], keys[pa])
			if e != nil {
				err = e
				goto fail
			}
			if less {
				keys[dest] = keys[pa]
				dest, pa = dest-1, pa-1
				acount, bcount = acount+1, 0
				na--
				if na == 0 {
					goto succeed
				}
				if acount >= s.minGallop {
					break
				}
			} else {
				keys[dest] = tmp[tb]
				dest, tb = dest-1, tb-1
				bcount, acount = bcount+1, 0
				nb--
				if nb == 1 {
					goto copyA
				}
				if bcount >= s.minGallop {
					break
				}
			}
		}
		s.minGallop++
		for {
			if s.minGallop > 1 {
				s.minGallop--
			}
			k, e := gallopRight(tmp[tb], keys[basea:basea+na], na-1)
			if e != nil {
				err = e
				goto fail
			}
			k = na - k
			acount = k
			if k > 0 {
				dest, pa = dest-k, pa-k
				copy(keys[dest+1:dest+1+k], keys[pa+1:pa+1+k])
				na -= k
				if na == 0 {
					goto succeed
				}
			}
			keys[dest] = tmp[tb]
			dest, tb, nb = dest-1, tb-1, nb-1
			if nb == 1 {
				goto copyA
			}
			k, e = gallopLeft(keys[pa], tmp[:nb], nb-1)
			if e != nil {
				err = e
				goto fail
			}
			k = nb - k
			bcount = k
			if k > 0 {
				dest, tb = dest-k, tb-k
				copy(keys[dest+1:dest+1+k], tmp[tb+1:tb+1+k])
				nb -= k
				if nb == 1 {
					goto copyA
				}
				if nb == 0 {
					goto succeed
				}
			}
			keys[dest] = keys[pa]
			dest, pa, na = dest-1, pa-1, na-1
			if na == 0 {
				goto succeed
			}
			if acount < pySortMinGallop && bcount < pySortMinGallop {
				break
			}
		}
		s.minGallop++
	}
succeed:
fail:
	if nb > 0 {
		copy(keys[dest-(nb-1):dest+1], tmp[:nb])
	}
	return err
copyA:
	copy(keys[dest+1-na:dest+1], keys[pa+1-na:pa+1])
	dest -= na
	keys[dest] = tmp[tb]
	return nil
}

// mergeAt is merge_at: merges the pending runs i and i+1.
func (s *pySort) mergeAt(i int) error {
	a, b := s.pending[i], s.pending[i+1]
	s.pending[i].length = a.length + b.length
	s.pending = append(s.pending[:i+1], s.pending[i+2:]...)
	k, err := gallopRight(s.keys[b.base], s.keys[a.base:a.base+a.length], 0)
	if err != nil {
		return err
	}
	pa, na := a.base+k, a.length-k
	if na == 0 {
		return nil
	}
	nb, err := gallopLeft(s.keys[pa+na-1], s.keys[b.base:b.base+b.length], b.length-1)
	if err != nil || nb == 0 {
		return err
	}
	if na <= nb {
		return s.mergeLo(pa, na, b.base, nb)
	}
	return s.mergeHi(pa, na, b.base, nb)
}

// pySortPower is powerloop: the depth of the boundary between two adjacent runs.
func pySortPower(s1, n1, n2, n int) int {
	result := 0
	a := 2*s1 + n1
	b := a + n1 + n2
	for {
		result++
		if a >= n {
			a -= n
			b -= n
		} else if b >= n {
			break
		}
		a <<= 1
		b <<= 1
	}
	return result
}

// foundNewRun is found_new_run: the powersort merges a new run of n2 triggers.
func (s *pySort) foundNewRun(n2 int) error {
	if len(s.pending) == 0 {
		return nil
	}
	top := s.pending[len(s.pending)-1]
	power := pySortPower(top.base, top.length, n2, len(s.keys))
	for len(s.pending) > 1 && s.pending[len(s.pending)-2].power > power {
		if err := s.mergeAt(len(s.pending) - 2); err != nil {
			return err
		}
	}
	s.pending[len(s.pending)-1].power = power
	return nil
}

func (s *pySort) forceCollapse() error {
	for len(s.pending) > 1 {
		n := len(s.pending) - 2
		if n > 0 && s.pending[n-1].length < s.pending[n+1].length {
			n--
		}
		if err := s.mergeAt(n); err != nil {
			return err
		}
	}
	return nil
}

// pyLess is Python's v < w over decoded JSON values: bool, int and float by
// exact value (NaN is never less nor greater), str by code point (WTF-8 bytes
// order as code points do), list lexicographically after the first unequal
// item, and anything else the TypeError Python raises for the pair.
func pyLess(v, w any) (bool, error) {
	if x, ok := pyReal(v); ok {
		if y, ok := pyReal(w); ok {
			return pyRealLess(x, y), nil
		}
	}
	switch x := v.(type) {
	case string:
		if y, ok := w.(string); ok {
			return x < y, nil
		}
	case []any:
		if y, ok := w.([]any); ok {
			for i := 0; i < len(x) && i < len(y); i++ {
				// Item equality tries identity first; json gives every NaN one object.
				if !evidence.Equal(x[i], y[i]) {
					return pyLess(x[i], y[i])
				}
			}
			return len(x) < len(y), nil
		}
	}
	return false, &evidence.PythonError{Class: "TypeError", Detail: fmt.Sprintf("'<' not supported between instances of '%s' and '%s'", evidence.TypeName(v), evidence.TypeName(w))}
}

// pyRealValue is a JSON number or bool: a float kept as float64 for NaN and
// the infinities, anything else as an exact rational.
type pyRealValue struct {
	float   float64
	isFloat bool
	exact   *big.Rat
}

func pyReal(v any) (pyRealValue, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return pyRealValue{exact: big.NewRat(1, 1)}, true
		}
		return pyRealValue{exact: new(big.Rat)}, true
	case int64:
		return pyRealValue{exact: new(big.Rat).SetInt64(x)}, true
	case json.Number: // an int beyond int64, as hook.Decode leaves it
		if n, ok := new(big.Int).SetString(string(x), 10); ok {
			return pyRealValue{exact: new(big.Rat).SetInt(n)}, true
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return pyRealValue{float: x, isFloat: true}, true
		}
		return pyRealValue{float: x, isFloat: true, exact: new(big.Rat).SetFloat64(x)}, true
	}
	return pyRealValue{}, false
}

func pyRealLess(x, y pyRealValue) bool {
	if x.isFloat && y.isFloat {
		return x.float < y.float
	}
	if x.isFloat && x.exact == nil {
		// NaN is unordered; -inf is below every int and +inf above.
		return math.IsInf(x.float, -1)
	}
	if y.isFloat && y.exact == nil {
		return math.IsInf(y.float, 1)
	}
	return x.exact.Cmp(y.exact) < 0
}
