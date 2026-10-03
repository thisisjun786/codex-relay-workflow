package skill

import (
	"bytes"
	"fmt"
)

// The union rule of a mechanical region (CRW-412): both sides add lines to one list. A resolution of a
// conflicted file is a union only when it keeps every line of both sides and nothing else. The check
// reads lines with their line ends, so a last line without a newline is a line of its own, and it
// never reads a resolution as anything it did not state: a side that changed or deleted a line of the
// base is not adding to a list, a line missing from the result or out of its side's order is lost, and
// a line the two sides do not give, or give fewer times, is added. A line both sides added must stand
// twice (the entry count of the result is the base's plus both sides' additions).

// unionStats are the counts a pass reports.
type unionStats struct{ base, previousAdded, devAdded, result int }

// unionFailure is a refusal of a resolution: the code names which part of the rule it broke.
type unionFailure struct{ code, detail string }

// splitLines is the lines of a file, each with its line end; a last line without one keeps none.
func splitLines(data []byte) []string {
	parts := bytes.SplitAfter(data, []byte("\n"))
	if n := len(parts); n > 0 && len(parts[n-1]) == 0 {
		parts = parts[:n-1]
	}
	lines := make([]string, len(parts))
	for i, p := range parts {
		lines[i] = string(p)
	}
	return lines
}

// firstUnmatched reads sub against seq greedily and answers the index of the first line of sub that
// seq does not hold after the lines before it, or -1 when sub is a subsequence of seq.
func firstUnmatched(sub, seq []string) int {
	at := 0
	for i, line := range sub {
		for at < len(seq) && seq[at] != line {
			at++
		}
		if at == len(seq) {
			return i
		}
		at++
	}
	return -1
}

func countLines(lines []string) map[string]int {
	counts := make(map[string]int, len(lines))
	for _, line := range lines {
		counts[line]++
	}
	return counts
}

// checkUnion decides whether result is the union of the two sides of a conflicted file over their base:
// previous is the side of the head that was verified, dev the side of the base tip. It returns the
// counts of a pass, or the refusal.
func checkUnion(base, previous, dev, result []byte) (unionStats, *unionFailure) {
	lb, lp, ld, ln := splitLines(base), splitLines(previous), splitLines(dev), splitLines(result)
	stats := unionStats{base: len(lb), previousAdded: len(lp) - len(lb), devAdded: len(ld) - len(lb), result: len(ln)}
	for _, side := range []struct {
		name  string
		lines []string
	}{{"previous head", lp}, {"dev tip", ld}} {
		if at := firstUnmatched(lb, side.lines); at >= 0 {
			return stats, &unionFailure{"union_not_additions", fmt.Sprintf("the %s changed or removed a line of the base (%q), so its side does not only add to the list; a union keeps the lines of a list that both sides extend, and this goes back to the child", side.name, lb[at])}
		}
	}
	for _, side := range []struct {
		name  string
		lines []string
	}{{"previous head", lp}, {"dev tip", ld}} {
		if at := firstUnmatched(side.lines, ln); at >= 0 {
			return stats, &unionFailure{"union_line_lost", fmt.Sprintf("a line of the %s's side is missing from the result or out of its side's order: %q", side.name, side.lines[at])}
		}
	}
	cb, cp, cd, cn := countLines(lb), countLines(lp), countLines(ld), countLines(ln)
	seen := map[string]bool{}
	for _, lines := range [][]string{ln, lp, ld} {
		for _, line := range lines {
			if seen[line] {
				continue
			}
			seen[line] = true
			want, got := cp[line]+cd[line]-cb[line], cn[line]
			switch {
			case got < want:
				return stats, &unionFailure{"union_line_lost", fmt.Sprintf("the line %q stands %d time(s) in the result but the two sides give it %d (a line both sides added is kept once for each)", line, got, want)}
			case got > want:
				return stats, &unionFailure{"union_line_added", fmt.Sprintf("the line %q stands %d time(s) in the result but the two sides give it %d: a union adds nothing of its own", line, got, want)}
			}
		}
	}
	return stats, nil
}
