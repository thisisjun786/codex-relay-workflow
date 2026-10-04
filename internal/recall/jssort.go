package recall

import "slices"

// JSSort sorts a dense slice using Node v24.20.0's V8 TimSort comparison
// schedule, including inconsistent and stateful numeric comparators. NaN means
// equality. Comparisons operate on a private snapshot; the receiver is written
// only after sorting succeeds. Holes, JS undefined and property accessors are
// outside a Go slice's representation.
//
// Adapted from deps/v8/third_party/v8/builtins/array-sort.tq:348-360,486-1280.
// Changes: typed Go slices, local state and overlapping copy replace Torque's
// arrays; the two gallop searches share control flow but retain argument order.
// Upstream copyright and license notices are retained below.
func JSSort[T any](values []T, compare func(T, T) float64) {
	if len(values) < 2 {
		return
	}
	s := jsSortState[T]{a: slices.Clone(values), cmp: compare, minGallop: jsMinGallop}
	minRun, remainder := len(values), 0
	for minRun >= 64 {
		remainder |= minRun & 1
		minRun >>= 1
	}
	minRun += remainder
	for low := 0; low < len(s.a); {
		n := s.countRun(low)
		if n < minRun {
			forced := min(minRun, len(s.a)-low)
			s.insertion(low, low+n, low+forced)
			n = forced
		}
		s.runs = append(s.runs, jsSortRun{low, n})
		s.collapse(false)
		low += n
	}
	s.collapse(true)
	copy(values, s.a)
}

const jsMinGallop = 7

type jsSortRun struct{ base, length int }

type jsSortState[T any] struct {
	a, temp   []T
	cmp       func(T, T) float64
	runs      []jsSortRun
	minGallop int
}

func (s *jsSortState[T]) compare(a, b T) float64 {
	v := s.cmp(a, b)
	if v != v { // SortCompareUserFn: NaN becomes +0 before any branch.
		return 0
	}
	return v
}

func (s *jsSortState[T]) countRun(low int) int {
	if low+1 == len(s.a) {
		return 1
	}
	n := 2
	descending := s.compare(s.a[low+1], s.a[low]) < 0
	for i := low + 2; i < len(s.a); i++ {
		if (s.compare(s.a[i], s.a[i-1]) < 0) != descending {
			break
		}
		n++
	}
	if descending {
		slices.Reverse(s.a[low : low+n])
	}
	return n
}

func (s *jsSortState[T]) insertion(low, start, high int) {
	if start == low {
		start++
	}
	for ; start < high; start++ {
		pivot, left, right := s.a[start], low, start
		for left < right {
			mid := left + (right-left)/2
			if s.compare(pivot, s.a[mid]) < 0 {
				right = mid
			} else {
				left = mid + 1
			}
		}
		copy(s.a[left+1:start+1], s.a[left:start])
		s.a[left] = pivot
	}
}

func (s *jsSortState[T]) invariant(n int) bool {
	return n < 2 || s.runs[n-2].length > s.runs[n-1].length+s.runs[n].length
}

func (s *jsSortState[T]) collapse(force bool) {
	for len(s.runs) > 1 {
		n := len(s.runs) - 2
		if force {
			if n > 0 && s.runs[n-1].length < s.runs[n+1].length {
				n--
			}
		} else if !s.invariant(n+1) || !s.invariant(n) {
			if s.runs[n-1].length < s.runs[n+1].length {
				n--
			}
		} else if s.runs[n].length > s.runs[n+1].length {
			break
		}
		s.mergeAt(n)
	}
}

func (s *jsSortState[T]) mergeAt(i int) {
	a, b := s.runs[i], s.runs[i+1]
	s.runs[i].length += b.length
	copy(s.runs[i+1:], s.runs[i+2:])
	s.runs = s.runs[:len(s.runs)-1]
	k := s.gallop(s.a, s.a[b.base], a.base, a.length, 0, false)
	a.base, a.length = a.base+k, a.length-k
	if a.length == 0 {
		return
	}
	b.length = s.gallop(s.a, s.a[a.base+a.length-1], b.base, b.length, b.length-1, true)
	if b.length == 0 {
		return
	}
	if a.length <= b.length {
		s.mergeLow(a, b)
	} else {
		s.mergeHigh(a, b)
	}
}

// left selects GallopLeft (element,key); right uses (key,element). Never
// negate a comparison to swap arguments: inconsistent comparators forbid it.
func (s *jsSortState[T]) gallop(array []T, key T, base, length, hint int, left bool) int {
	compare := func(i int) float64 {
		if left {
			return s.compare(array[base+i], key)
		}
		return s.compare(key, array[base+i])
	}
	last, offset := 0, 1
	if (compare(hint) < 0) == left {
		maxOffset := length - hint
		for offset < maxOffset {
			if (compare(hint+offset) >= 0) == left {
				break
			}
			last, offset = offset, (offset<<1)+1
			if offset <= 0 {
				offset = maxOffset
			}
		}
		offset = min(offset, maxOffset)
		last, offset = last+hint, offset+hint
	} else {
		maxOffset := hint + 1
		for offset < maxOffset {
			if (compare(hint-offset) < 0) == left {
				break
			}
			last, offset = offset, (offset<<1)+1
			if offset <= 0 {
				offset = maxOffset
			}
		}
		offset = min(offset, maxOffset)
		last, offset = hint-offset, hint-last
	}
	last++
	for last < offset {
		mid := last + (offset-last)/2
		if (compare(mid) < 0) == left {
			last = mid + 1
		} else {
			offset = mid
		}
	}
	return offset
}

func (s *jsSortState[T]) buffer(n int) []T {
	if len(s.temp) < n {
		s.temp = make([]T, max(n, 32))
	}
	return s.temp[:n]
}

// The labels mirror V8's exits, including runs exhausted by an inconsistent
// comparator. Splitting these loops would obscure the adaptive gallop state.
func (s *jsSortState[T]) mergeLow(a, b jsSortRun) {
	temp := s.buffer(a.length)
	copy(temp, s.a[a.base:a.base+a.length])
	lengthA, lengthB := a.length, b.length
	dest, cursorTemp, cursorB := a.base, 0, b.base
	minGallop := s.minGallop
	s.a[dest] = s.a[cursorB]
	dest++
	cursorB++
	lengthB--
	if lengthB == 0 {
		goto succeed
	}
	if lengthA == 1 {
		goto copyB
	}
	for {
		winsA, winsB := 0, 0
		for {
			if s.compare(s.a[cursorB], temp[cursorTemp]) < 0 {
				s.a[dest] = s.a[cursorB]
				dest++
				cursorB++
				winsB++
				lengthB--
				winsA = 0
				if lengthB == 0 {
					goto succeed
				}
				if winsB >= minGallop {
					break
				}
			} else {
				s.a[dest] = temp[cursorTemp]
				dest++
				cursorTemp++
				winsA++
				lengthA--
				winsB = 0
				if lengthA == 1 {
					goto copyB
				}
				if winsA >= minGallop {
					break
				}
			}
		}
		minGallop++
		for first := true; first || winsA >= jsMinGallop || winsB >= jsMinGallop; {
			first = false
			minGallop = max(1, minGallop-1)
			s.minGallop = minGallop
			winsA = s.gallop(temp, s.a[cursorB], cursorTemp, lengthA, 0, false)
			if winsA > 0 {
				copy(s.a[dest:dest+winsA], temp[cursorTemp:cursorTemp+winsA])
				dest, cursorTemp, lengthA = dest+winsA, cursorTemp+winsA, lengthA-winsA
				if lengthA == 1 {
					goto copyB
				}
				if lengthA == 0 { // V8 :973, reachable for inconsistent comparisons.
					goto succeed
				}
			}
			s.a[dest] = s.a[cursorB]
			dest++
			cursorB++
			lengthB--
			if lengthB == 0 {
				goto succeed
			}
			winsB = s.gallop(s.a, temp[cursorTemp], cursorB, lengthB, 0, true)
			if winsB > 0 {
				copy(s.a[dest:dest+winsB], s.a[cursorB:cursorB+winsB])
				dest, cursorB, lengthB = dest+winsB, cursorB+winsB, lengthB-winsB
				if lengthB == 0 {
					goto succeed
				}
			}
			s.a[dest] = temp[cursorTemp]
			dest++
			cursorTemp++
			lengthA--
			if lengthA == 1 {
				goto copyB
			}
		}
		minGallop++
		s.minGallop = minGallop
	}
succeed:
	copy(s.a[dest:dest+lengthA], temp[cursorTemp:cursorTemp+lengthA])
	return
copyB:
	copy(s.a[dest:dest+lengthB], s.a[cursorB:cursorB+lengthB])
	s.a[dest+lengthB] = temp[cursorTemp]
}

func (s *jsSortState[T]) mergeHigh(a, b jsSortRun) {
	temp := s.buffer(b.length)
	copy(temp, s.a[b.base:b.base+b.length])
	lengthA, lengthB := a.length, b.length
	dest, cursorTemp, cursorA := b.base+b.length-1, b.length-1, a.base+a.length-1
	minGallop := s.minGallop
	s.a[dest] = s.a[cursorA]
	dest--
	cursorA--
	lengthA--
	if lengthA == 0 {
		goto succeed
	}
	if lengthB == 1 {
		goto copyA
	}
	for {
		winsA, winsB := 0, 0
		for {
			if s.compare(temp[cursorTemp], s.a[cursorA]) < 0 {
				s.a[dest] = s.a[cursorA]
				dest--
				cursorA--
				winsA++
				lengthA--
				winsB = 0
				if lengthA == 0 {
					goto succeed
				}
				if winsA >= minGallop {
					break
				}
			} else {
				s.a[dest] = temp[cursorTemp]
				dest--
				cursorTemp--
				winsB++
				lengthB--
				winsA = 0
				if lengthB == 1 {
					goto copyA
				}
				if winsB >= minGallop {
					break
				}
			}
		}
		minGallop++
		for first := true; first || winsA >= jsMinGallop || winsB >= jsMinGallop; {
			first = false
			minGallop = max(1, minGallop-1)
			s.minGallop = minGallop
			k := s.gallop(s.a, temp[cursorTemp], a.base, lengthA, lengthA-1, false)
			winsA = lengthA - k
			if winsA > 0 {
				dest, cursorA = dest-winsA, cursorA-winsA
				copy(s.a[dest+1:dest+1+winsA], s.a[cursorA+1:cursorA+1+winsA])
				lengthA -= winsA
				if lengthA == 0 {
					goto succeed
				}
			}
			s.a[dest] = temp[cursorTemp]
			dest--
			cursorTemp--
			lengthB--
			if lengthB == 1 {
				goto copyA
			}
			k = s.gallop(temp, s.a[cursorA], 0, lengthB, lengthB-1, true)
			winsB = lengthB - k
			if winsB > 0 {
				dest, cursorTemp = dest-winsB, cursorTemp-winsB
				copy(s.a[dest+1:dest+1+winsB], temp[cursorTemp+1:cursorTemp+1+winsB])
				lengthB -= winsB
				if lengthB == 1 {
					goto copyA
				}
				if lengthB == 0 { // V8 :1124, reachable for inconsistent comparisons.
					goto succeed
				}
			}
			s.a[dest] = s.a[cursorA]
			dest--
			cursorA--
			lengthA--
			if lengthA == 0 {
				goto succeed
			}
		}
		minGallop++
		s.minGallop = minGallop
	}
succeed:
	copy(s.a[dest-(lengthB-1):dest+1], temp[:lengthB])
	return
copyA:
	dest, cursorA = dest-lengthA, cursorA-lengthA
	copy(s.a[dest+1:dest+1+lengthA], s.a[cursorA+1:cursorA+1+lengthA])
	s.a[dest] = temp[cursorTemp]
}

/*
Copyright (c) 2001-2018 Python Software Foundation; All Rights Reserved.

Copyright 2014, the V8 project authors. All rights reserved.
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

    * Redistributions of source code must retain the above copyright
      notice, this list of conditions and the following disclaimer.
    * Redistributions in binary form must reproduce the above
      copyright notice, this list of conditions and the following
      disclaimer in the documentation and/or other materials provided
      with the distribution.
    * Neither the name of Google Inc. nor the names of its
      contributors may be used to endorse or promote products derived
      from this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

PYTHON SOFTWARE FOUNDATION LICENSE VERSION 2
--------------------------------------------

1. This LICENSE AGREEMENT is between the Python Software Foundation
("PSF"), and the Individual or Organization ("Licensee") accessing and
otherwise using this software ("Python") in source or binary form and
its associated documentation.

2. Subject to the terms and conditions of this License Agreement, PSF hereby
grants Licensee a nonexclusive, royalty-free, world-wide license to reproduce,
analyze, test, perform and/or display publicly, prepare derivative works,
distribute, and otherwise use Python alone or in any derivative version,
provided, however, that PSF's License Agreement and PSF's notice of copyright,
i.e., "Copyright (c) 2001, 2002, 2003, 2004, 2005, 2006, 2007, 2008, 2009, 2010,
2011, 2012, 2013, 2014, 2015, 2016, 2017, 2018 Python Software Foundation; All
Rights Reserved" are retained in Python alone or in any derivative version
prepared by Licensee.

3. In the event Licensee prepares a derivative work that is based on
or incorporates Python or any part thereof, and wants to make
the derivative work available to others as provided herein, then
Licensee hereby agrees to include in any such work a brief summary of
the changes made to Python.

4. PSF is making Python available to Licensee on an "AS IS"
basis.  PSF MAKES NO REPRESENTATIONS OR WARRANTIES, EXPRESS OR
IMPLIED.  BY WAY OF EXAMPLE, BUT NOT LIMITATION, PSF MAKES NO AND
DISCLAIMS ANY REPRESENTATION OR WARRANTY OF MERCHANTABILITY OR FITNESS
FOR ANY PARTICULAR PURPOSE OR THAT THE USE OF PYTHON WILL NOT
INFRINGE ANY THIRD PARTY RIGHTS.

5. PSF SHALL NOT BE LIABLE TO LICENSEE OR ANY OTHER USERS OF PYTHON
FOR ANY INCIDENTAL, SPECIAL, OR CONSEQUENTIAL DAMAGES OR LOSS AS
A RESULT OF MODIFYING, DISTRIBUTING, OR OTHERWISE USING PYTHON,
OR ANY DERIVATIVE THEREOF, EVEN IF ADVISED OF THE POSSIBILITY THEREOF.

6. This License Agreement will automatically terminate upon a material
breach of its terms and conditions.

7. Nothing in this License Agreement shall be deemed to create any
relationship of agency, partnership, or joint venture between PSF and
Licensee.  This License Agreement does not grant permission to use PSF
trademarks or trade name in a trademark sense to endorse or promote
products or services of Licensee, or any third party.

8. By copying, installing or otherwise using Python, Licensee
agrees to be bound by the terms and conditions of this License
Agreement.

*/
