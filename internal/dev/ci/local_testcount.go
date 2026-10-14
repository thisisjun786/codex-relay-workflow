//go:build dev

package ci

import (
	"regexp"
	"strings"
)

// CRW-1328: the count of test packages a run's steps ran tests in, read from the package result lines go test prints. go test exits 0
// when no test ran (a -run pattern that matches nothing, a package whose tests a build tag leaves out), and without -v it prints no
// per-test count, so the record counts packages: "ok <package> <time>" and "FAIL <package> <time>" ran tests; "[no tests to run]",
// "? <package> [no test files]" and "FAIL <package> [build failed]" ran none. Other tools' lines (a TAP "ok 1 - name") carry no
// duration and are not read. A step whose output holds no package result line counts nothing.
var (
	localTestRanLine  = regexp.MustCompile(`^(ok|FAIL)\s+\S+\s+(\d+(\.\d+)?s|\(cached\))(.*)$`)
	localTestNoneLine = regexp.MustCompile(`^(\?\s+\S+\s+\[no test files\]|FAIL\s+\S+\s+\[(build|setup) failed\])`)
)

// localTestPackagesRun counts the packages of go test output whose tests ran, and says whether the output held a package result line.
func localTestPackagesRun(output string) (ran int, saw bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := localTestRanLine.FindStringSubmatch(line); m != nil {
			saw = true
			if !strings.Contains(m[4], "[no tests to run]") {
				ran++
			}
		} else if localTestNoneLine.MatchString(line) {
			saw = true
		}
	}
	return ran, saw
}
