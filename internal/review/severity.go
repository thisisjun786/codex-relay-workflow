package review

import (
	"strings"
	"unicode"
)

// NormalizeGrade maps a severity as a reviewer wrote it to a grade and the security flag. The text is lower-cased and split into
// words at every character that is not a letter or digit; p0, blocker and critical mean P0, p1, high and major P1, p2, medium,
// moderate and warning P2, p3, low, minor, nit and info P3, and security, vulnerability, vuln and sec set the flag. With several
// grade words the most severe wins. ok is false when no grade word is present (the grade is then empty and the flag may still be
// set): that is the defined outcome for an unknown severity, and Rules.Apply drops such a finding as unknown_grade.
func NormalizeGrade(raw string) (grade Grade, security, ok bool) {
	for _, word := range strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		var g Grade
		switch word {
		case "p0", "blocker", "critical":
			g = P0
		case "p1", "high", "major":
			g = P1
		case "p2", "medium", "moderate", "warning":
			g = P2
		case "p3", "low", "minor", "nit", "info":
			g = P3
		case "security", "vulnerability", "vuln", "sec":
			security = true
		}
		if g != "" && (grade == "" || g < grade) {
			grade = g
		}
	}
	return grade, security, grade != ""
}
