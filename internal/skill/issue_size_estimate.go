package skill

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The estimate signal of "crw skill issue-size check" and the "calibrate" command (CRW-739).
//
// A body that states a line estimate is scaled by the measured actual/estimate ratios of the
// calibration table: when the stated estimate times the 75th-percentile ratio exceeds the ceiling,
// the decision becomes split_recommended with the reason estimate_scaled_over_ceiling. The ratios
// are computed from the table on every run (no number is baked into the code) and the percentile
// arithmetic is exact integer fractions, so the same table and the same body are the same bytes.
//
// Every package-level name here is prefixed with sizeEstimate (the runIssueSizeCalibrate command
// follows the family's runIssueSize naming) so it cannot collide with the package's other work.

const (
	sizeEstimateCalibrateSchema = "crw-issue-size-calibrate/1"

	// sizeEstimateDefaultCeiling is the ceiling an input that states none gets.
	sizeEstimateDefaultCeiling = 600

	// The percentiles the calibration table yields, as the exact fractions k/d: the median (1/2) and
	// the 75th percentile (3/4).
	sizeEstimateMedianNum, sizeEstimateMedianDen = 1, 2
	sizeEstimateP75Num, sizeEstimateP75Den       = 3, 4
)

// sizeEstimateCalibration is the measured table built into the command: the issues whose bodies
// stated a line estimate and what their pull requests turned out to be. A new measurement arrives as
// a pull request that edits this file.
//
//go:embed testdata/issue_size_calibration.json
var sizeEstimateCalibration []byte

// sizeEstimateCalibrationRow is one measured issue: the body's stated estimate (its lower and upper
// bound, null where the body stated none) and the actual implementation and test lines of its
// delivery.
type sizeEstimateCalibrationRow struct {
	Issue        string `json:"issue"`
	EstimateLow  *int64 `json:"estimate_low"`
	EstimateHigh *int64 `json:"estimate_high"`
	ActualImpl   *int64 `json:"actual_impl"`
	ActualTest   *int64 `json:"actual_test"`
}

type sizeEstimateCalibrationFile struct {
	Schema string                       `json:"schema"`
	Rows   []sizeEstimateCalibrationRow `json:"rows"`
}

// sizeEstimateRatio is one row's actual/estimate as the exact fraction num/den.
type sizeEstimateRatio struct {
	issue string
	num   int64 // actual implementation + actual test
	den   int64 // the stated estimate (the range's upper bound)
}

// sizeEstimateDistribution is a calibration table's ratio distribution.
type sizeEstimateDistribution struct {
	rows []sizeEstimateRatio
	p50  sizeEstimateRatio // the median
	p75  sizeEstimateRatio // the 75th percentile
}

// sizeEstimateReport is the check report's estimate object.
type sizeEstimateReport struct {
	Stated    int64   `json:"stated"`
	P50Ratio  float64 `json:"p50_ratio"`
	P75Ratio  float64 `json:"p75_ratio"`
	ScaledP75 float64 `json:"scaled_p75"`
	Ceiling   int64   `json:"ceiling"`
	TableRows int     `json:"table_rows"`
}

// sizeEstimateCalibrateRatio is one row of the calibrate command's output.
type sizeEstimateCalibrateRatio struct {
	Issue    string  `json:"issue"`
	Estimate int64   `json:"estimate"`
	Actual   int64   `json:"actual"`
	Ratio    float64 `json:"ratio"`
}

// sizeEstimateCalibrateReport is the calibrate command's output: the table's ratio distribution.
type sizeEstimateCalibrateReport struct {
	Schema    string                       `json:"schema"`
	TableRows int                          `json:"table_rows"`
	Ratios    []sizeEstimateCalibrateRatio `json:"ratios"`
	P50Ratio  float64                      `json:"p50_ratio"`
	P75Ratio  float64                      `json:"p75_ratio"`
}

// sizeEstimateRatioFloat is num/den as a float64. IEEE-754 division is correctly rounded and the
// same on every platform, so one fraction is one set of bytes.
func sizeEstimateRatioFloat(num, den int64) float64 { return float64(num) / float64(den) }

// sizeEstimateReduce divides a fraction by its greatest common divisor.
func sizeEstimateReduce(num, den int64) (int64, int64) {
	a, b := num, den
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return num, den
	}
	return num / a, den / a
}

// sizeEstimateReadCalibration parses a table and answers its rows as ratios. A row whose estimate or
// actual cells are not stated is refused, and a table with no row is refused, so an incomplete table
// is never read as zero.
func sizeEstimateReadCalibration(raw []byte) ([]sizeEstimateRatio, error) {
	if !utf8.Valid(raw) {
		return nil, errNotUTF8
	}
	var table sizeEstimateCalibrationFile
	if err := json.Unmarshal(raw, &table); err != nil {
		return nil, err
	}
	if len(table.Rows) == 0 {
		return nil, errors.New("the table states no rows")
	}
	rows := make([]sizeEstimateRatio, 0, len(table.Rows))
	for _, row := range table.Rows {
		switch {
		case row.EstimateHigh == nil:
			return nil, fmt.Errorf("row %q states no estimate_high", row.Issue)
		case row.ActualImpl == nil || row.ActualTest == nil:
			return nil, fmt.Errorf("row %q states no actual_impl or actual_test", row.Issue)
		case *row.EstimateHigh <= 0:
			return nil, fmt.Errorf("row %q has a non-positive estimate_high", row.Issue)
		}
		rows = append(rows, sizeEstimateRatio{issue: row.Issue, num: *row.ActualImpl + *row.ActualTest, den: *row.EstimateHigh})
	}
	return rows, nil
}

// sizeEstimatePercentile is the value at the fraction k/d of a sorted ratio list, by linear
// interpolation (the R type 7 definition), as an exact fraction. Integer arithmetic only, so the
// answer cannot depend on a floating-point rounding.
func sizeEstimatePercentile(sorted []sizeEstimateRatio, k, d int64) sizeEstimateRatio {
	total := int64(len(sorted)-1) * k
	index, rem := int(total/d), total%d
	if rem == 0 {
		return sorted[index]
	}
	a, b := sorted[index], sorted[index+1]
	// a + (b-a)*rem/d, over a common denominator.
	num, den := sizeEstimateReduce(a.num*b.den*d+(b.num*a.den-a.num*b.den)*rem, a.den*b.den*d)
	return sizeEstimateRatio{num: num, den: den}
}

// sizeEstimateDistributionOf orders a table's ratios by cross-multiplication and answers their
// median and 75th percentile.
func sizeEstimateDistributionOf(rows []sizeEstimateRatio) sizeEstimateDistribution {
	sorted := append([]sizeEstimateRatio(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].num*sorted[j].den < sorted[j].num*sorted[i].den
	})
	return sizeEstimateDistribution{
		rows: rows,
		p50:  sizeEstimatePercentile(sorted, sizeEstimateMedianNum, sizeEstimateMedianDen),
		p75:  sizeEstimatePercentile(sorted, sizeEstimateP75Num, sizeEstimateP75Den),
	}
}

// sizeEstimateForms are the ways a body states its line estimate, in the sections the check reads.
// A range states both bounds and the larger one wins, so the stated estimate is the range's upper
// bound. The forms are the ones the issue names: 추정 N줄, 약 N줄, N~M줄, estimate N lines, about N lines.
var sizeEstimateForms = []*regexp.Regexp{
	regexp.MustCompile("(?:추정|약)[[:space:]]*([0-9][0-9,]*)[[:space:]]*줄"),
	regexp.MustCompile("([0-9][0-9,]*)[[:space:]]*[~–—-][[:space:]]*([0-9][0-9,]*)[[:space:]]*줄"),
	regexp.MustCompile("(?i)(?:estimate[sd]?|about)[[:space:]]+(?:of[[:space:]]+)?(?:about[[:space:]]+)?([0-9][0-9,]*)[[:space:]]*lines?"),
}

// sizeEstimateStated is the single largest line estimate the text states in the forms the check
// reads, or 0 when it states none.
func sizeEstimateStated(text string) int64 {
	var largest int64
	for _, form := range sizeEstimateForms {
		for _, match := range form.FindAllStringSubmatch(text, -1) {
			for _, group := range match[1:] {
				if group == "" {
					continue
				}
				if n, err := strconv.ParseInt(strings.ReplaceAll(group, ",", ""), 10, 64); err == nil && n > largest {
					largest = n
				}
			}
		}
	}
	return largest
}

// sizeEstimateText is the body text the estimate is read from: the scope, size and deliverables
// sections (and the same fields when the input gives them instead of a body).
func sizeEstimateText(issue sizeIssue) string {
	parts := make([]string, 0, len(issue.scope)+len(issue.deliverables))
	parts = append(parts, issue.scope...)
	parts = append(parts, issue.deliverables...)
	return strings.Join(parts, "\n")
}

// sizeEstimateSignal reads the body's stated estimate and scales it by the calibration table's
// ratios. It answers nil when the body states none, so an issue without an estimate keeps the answer
// it had. overCeiling is the exact comparison stated x p75 > ceiling.
func sizeEstimateSignal(issue sizeIssue, ceiling int64) (report *sizeEstimateReport, overCeiling bool, err error) {
	stated := sizeEstimateStated(sizeEstimateText(issue))
	if stated <= 0 {
		return nil, false, nil
	}
	rows, err := sizeEstimateReadCalibration(sizeEstimateCalibration)
	if err != nil {
		return nil, false, fmt.Errorf("the calibration table: %w", err)
	}
	dist := sizeEstimateDistributionOf(rows)
	return &sizeEstimateReport{
		Stated:    stated,
		P50Ratio:  sizeEstimateRatioFloat(dist.p50.num, dist.p50.den),
		P75Ratio:  sizeEstimateRatioFloat(dist.p75.num, dist.p75.den),
		ScaledP75: sizeEstimateRatioFloat(stated*dist.p75.num, dist.p75.den),
		Ceiling:   ceiling,
		TableRows: len(rows),
	}, stated*dist.p75.num > ceiling*dist.p75.den, nil
}

// sizeEstimateReason is the reason a scaled estimate over the ceiling adds.
func sizeEstimateReason(e *sizeEstimateReport) string {
	return fmt.Sprintf("estimate_scaled_over_ceiling: the stated estimate %d lines scaled by the p75 ratio %v is %v, over the ceiling %d", e.Stated, e.P75Ratio, e.ScaledP75, e.Ceiling)
}

// sizeEstimateCeiling is the ceiling the input states, or the default when it states none. A
// negative ceiling is refused: it would flag every body that states an estimate.
func sizeEstimateCeiling(stated int) (int64, error) {
	switch {
	case stated < 0:
		return 0, fmt.Errorf("size_ceiling %d is negative", stated)
	case stated == 0:
		return sizeEstimateDefaultCeiling, nil
	}
	return int64(stated), nil
}

// runIssueSizeCalibrate reads a measured table and prints its ratio distribution.
func runIssueSizeCalibrate(args []string, stdout, stderr io.Writer) int {
	line := newCommandLine("issue-size", "calibrate", "Read a measured table and print the actual/estimate ratio distribution as JSON.")
	table := line.String("table", "", "the measured table to read (default: the one built into crw)")
	_, code := line.parse(args, stdout, stderr)
	if code >= 0 {
		return code
	}
	raw := sizeEstimateCalibration
	if *table != "" {
		data, err := os.ReadFile(*table)
		if err != nil {
			fmt.Fprintf(stderr, "Calibration failed: %s. Nothing was written.\n", err)
			return 3
		}
		raw = data
	}
	rows, err := sizeEstimateReadCalibration(raw)
	if err != nil {
		fmt.Fprintln(stderr, "Unreadable table: "+err.Error())
		return 2
	}
	dist := sizeEstimateDistributionOf(rows)
	out := sizeEstimateCalibrateReport{Schema: sizeEstimateCalibrateSchema, TableRows: len(rows), Ratios: make([]sizeEstimateCalibrateRatio, 0, len(rows)), P50Ratio: sizeEstimateRatioFloat(dist.p50.num, dist.p50.den), P75Ratio: sizeEstimateRatioFloat(dist.p75.num, dist.p75.den)}
	for _, row := range rows {
		out.Ratios = append(out.Ratios, sizeEstimateCalibrateRatio{Issue: row.issue, Estimate: row.den, Actual: row.num, Ratio: sizeEstimateRatioFloat(row.num, row.den)})
	}
	_ = emit(stdout, out)
	return 0
}
