package pyoracle

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// internedPrefix opens an answer that Intern encoded. No Python answer begins with it.
const internedPrefix = "pyoracle-interned-v1\n"

// pieceSeparator ends each stored piece. An answer that holds it is not encoded.
const pieceSeparator = '\x1e'

// Intern encodes an answer that repeats the same text many times far apart, such as a table dump
// taken after every operation of a scenario, so its recording stays small: gzip looks back only
// 32 KiB and so cannot fold such repeats. The answer is cut after every '}' and newline; each
// distinct piece is stored once, in order of first appearance, and the answer becomes runs of
// consecutive pieces. The encoding is the prefix, one line of runs written "first+count" or, for
// a run repeated back to back, "first+count*times", then the pieces, each followed by an ASCII
// record separator. Pieces are stored as they are, so a Substitute placeholder for a string
// without '}' or a newline applies to the encoded answer as to the plain one. Expand undoes the
// encoding exactly, whatever the answer's format. An answer that is not UTF-8 or that holds the
// separator is returned as it is.
func Intern(answer []byte) []byte {
	if !utf8.Valid(answer) || bytes.IndexByte(answer, pieceSeparator) >= 0 {
		return answer
	}
	index := map[string]int{}
	var pieces []string
	var runs [][2]int
	for rest := answer; len(rest) > 0; {
		end := bytes.IndexAny(rest, "}\n") + 1
		if end == 0 {
			end = len(rest)
		}
		piece := string(rest[:end])
		rest = rest[end:]
		at, seen := index[piece]
		if !seen {
			at = len(pieces)
			index[piece] = at
			pieces = append(pieces, piece)
		}
		if n := len(runs); n > 0 && runs[n-1][0]+runs[n-1][1] == at {
			runs[n-1][1]++
		} else {
			runs = append(runs, [2]int{at, 1})
		}
	}
	var out bytes.Buffer
	out.WriteString(internedPrefix)
	for i := 0; i < len(runs); {
		times := 1
		for i+times < len(runs) && runs[i+times] == runs[i] {
			times++
		}
		if i > 0 {
			out.WriteByte(' ')
		}
		out.WriteString(strconv.Itoa(runs[i][0]) + "+" + strconv.Itoa(runs[i][1]))
		if times > 1 {
			out.WriteString("*" + strconv.Itoa(times))
		}
		i += times
	}
	out.WriteByte('\n')
	for _, piece := range pieces {
		out.WriteString(piece)
		out.WriteByte(pieceSeparator)
	}
	return out.Bytes()
}

// internAbove is the answer size from which AnswerInterned records the encoding, when the
// encoding is under half the answer; a smaller answer stays readable in its recording.
const internAbove = 32 << 10

// AnswerInterned is Answer for an answer that repeats itself, such as whole-table dumps taken
// after every step: a large answer is recorded as Intern encodes it, and the answer is returned
// as it was. Check mode compares the recorded and the live answer as they are stored, which are
// equal exactly when the answers are; a SameWhen comparator sees them as stored too.
func AnswerInterned(t testing.TB, key string, capture func() ([]byte, error), opts ...Option) []byte {
	t.Helper()
	encoded := Answer(t, key, func() ([]byte, error) {
		answer, err := capture()
		if err != nil {
			return nil, err
		}
		if len(answer) < internAbove {
			return answer, nil
		}
		if encoded := Intern(answer); len(encoded) < len(answer)/2 {
			return encoded, nil
		}
		return answer, nil
	}, opts...)
	answer, err := Expand(encoded)
	if err != nil {
		t.Fatalf("pyoracle: %q: %v", key, err)
	}
	return answer
}

// Expand returns the answer Intern encoded. An answer Intern did not encode is returned as it is.
func Expand(answer []byte) ([]byte, error) {
	body, found := bytes.CutPrefix(answer, []byte(internedPrefix))
	if !found {
		return answer, nil
	}
	line, stored, found := bytes.Cut(body, []byte("\n"))
	if !found {
		return nil, errors.New("pyoracle: expand: no line of runs")
	}
	pieces := bytes.Split(stored, []byte{pieceSeparator})
	if len(pieces) == 0 || len(pieces[len(pieces)-1]) != 0 {
		return nil, errors.New("pyoracle: expand: the last piece is not terminated")
	}
	pieces = pieces[:len(pieces)-1]
	var out bytes.Buffer
	for _, run := range strings.Fields(string(line)) {
		span, repeat, repeated := strings.Cut(run, "*")
		first, count, found := strings.Cut(span, "+")
		from, err1 := strconv.Atoi(first)
		n, err2 := strconv.Atoi(count)
		times, err3 := 1, error(nil)
		if repeated {
			times, err3 = strconv.Atoi(repeat)
		}
		if !found || err1 != nil || err2 != nil || err3 != nil || from < 0 || n < 0 || times < 1 || from+n > len(pieces) {
			return nil, fmt.Errorf("pyoracle: expand: run %q names a piece the answer does not hold", run)
		}
		for range times {
			for _, piece := range pieces[from : from+n] {
				out.Write(piece)
			}
		}
	}
	return out.Bytes(), nil
}
