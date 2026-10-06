package skill

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
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
// arithmetic is exact rational arithmetic, so the same table and body are the same bytes and no
// measurement can overflow it. Package-level names carry the sizeEstimate prefix (the calibrate
// command follows the family's runIssueSize naming).

const (
	sizeEstimateCalibrateSchema = "crw-issue-size-calibrate/1"
	sizeEstimateTableSchema     = "crw-issue-size-calibration/1"

	// sizeEstimateDefaultCeiling is the ceiling an input that states none gets.
	sizeEstimateDefaultCeiling = 600

	// The percentiles the table yields, as exact fractions k/d: the median and the 75th percentile.
	sizeEstimateMedianNum, sizeEstimateMedianDen = 1, 2
	sizeEstimateP75Num, sizeEstimateP75Den       = 3, 4
)

// sizeEstimateCalibration is the measured table built into the command; a new measurement arrives as
// a pull request that edits this file.
//
//go:embed testdata/issue_size_calibration.json
var sizeEstimateCalibration []byte

// sizeEstimateCalibrationRow is one measured issue: the body's stated estimate bounds (null where
// none was stated) and the actual implementation and test lines of its delivery.
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

// sizeEstimateRatio is one row's actual/estimate as an exact rational.
type sizeEstimateRatio struct {
	issue    string
	actual   int64
	estimate int64
	value    *big.Rat
}

// sizeEstimateDistribution is a calibration table's median and 75th-percentile ratios.
type sizeEstimateDistribution struct{ p50, p75 *big.Rat }

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

// sizeEstimateCalibrateReport is the calibrate command's output.
type sizeEstimateCalibrateReport struct {
	Schema    string                       `json:"schema"`
	TableRows int                          `json:"table_rows"`
	Ratios    []sizeEstimateCalibrateRatio `json:"ratios"`
	P50Ratio  float64                      `json:"p50_ratio"`
	P75Ratio  float64                      `json:"p75_ratio"`
}

// sizeEstimateFloat is a rational as the nearest float64; IEEE-754 rounding is the same everywhere.
func sizeEstimateFloat(r *big.Rat) float64 { f, _ := r.Float64(); return f }

// sizeEstimateReadCalibration parses a table and answers its rows as ratios. A table that is not this
// schema, has no rows, or has a row whose estimate or actual cells are missing, negative or would
// overflow is refused, so a malformed table is never read as zero or as a negative ratio.
func sizeEstimateReadCalibration(raw []byte) ([]sizeEstimateRatio, error) {
	if !utf8.Valid(raw) {
		return nil, errNotUTF8
	}
	var table sizeEstimateCalibrationFile
	if err := json.Unmarshal(raw, &table); err != nil {
		return nil, err
	}
	if table.Schema != sizeEstimateTableSchema {
		return nil, fmt.Errorf("the table schema is %q, not %q", table.Schema, sizeEstimateTableSchema)
	}
	if len(table.Rows) == 0 {
		return nil, errors.New("the table states no rows")
	}
	rows := make([]sizeEstimateRatio, 0, len(table.Rows))
	for _, row := range table.Rows {
		switch {
		case row.EstimateHigh == nil:
			return nil, fmt.Errorf("row %q states no estimate_high", row.Issue)
		case *row.EstimateHigh <= 0:
			return nil, fmt.Errorf("row %q has a non-positive estimate_high", row.Issue)
		case row.ActualImpl == nil || row.ActualTest == nil:
			return nil, fmt.Errorf("row %q states no actual_impl or actual_test", row.Issue)
		case *row.ActualImpl < 0 || *row.ActualTest < 0:
			return nil, fmt.Errorf("row %q has a negative actual line count", row.Issue)
		case *row.ActualImpl > math.MaxInt64-*row.ActualTest:
			return nil, fmt.Errorf("row %q's actual line counts overflow", row.Issue)
		}
		actual := *row.ActualImpl + *row.ActualTest
		rows = append(rows, sizeEstimateRatio{row.Issue, actual, *row.EstimateHigh, big.NewRat(actual, *row.EstimateHigh)})
	}
	return rows, nil
}

// sizeEstimatePercentile is the value at the fraction k/d of a sorted ratio list, by linear
// interpolation (the R type 7 definition); big.Rat keeps it exact.
func sizeEstimatePercentile(sorted []sizeEstimateRatio, k, d int64) *big.Rat {
	total := int64(len(sorted)-1) * k
	index, rem := int(total/d), total%d
	if rem == 0 {
		return sorted[index].value
	}
	a, b := sorted[index].value, sorted[index+1].value
	step := new(big.Rat).Sub(b, a)
	step.Mul(step, new(big.Rat).SetFrac(big.NewInt(rem), big.NewInt(d)))
	return new(big.Rat).Add(a, step)
}

// sizeEstimateDistributionOf orders a table's ratios and answers their median and 75th percentile.
func sizeEstimateDistributionOf(rows []sizeEstimateRatio) sizeEstimateDistribution {
	sorted := append([]sizeEstimateRatio(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].value.Cmp(sorted[j].value) < 0 })
	return sizeEstimateDistribution{
		p50: sizeEstimatePercentile(sorted, sizeEstimateMedianNum, sizeEstimateMedianDen),
		p75: sizeEstimatePercentile(sorted, sizeEstimateP75Num, sizeEstimateP75Den),
	}
}

// sizeEstimateForms are the ways a body states its line estimate, in the sections the check reads:
// 추정 N줄, 약 N줄, N~M줄, estimate N lines, about N lines. A range's upper bound is the stated value.
var sizeEstimateForms = []*regexp.Regexp{
	regexp.MustCompile("(?:추정|약)[[:space:]]*([0-9][0-9,]*)[[:space:]]*줄"),
	regexp.MustCompile("([0-9][0-9,]*)[[:space:]]*[~–—-][[:space:]]*([0-9][0-9,]*)[[:space:]]*줄"),
	regexp.MustCompile("(?i)(?:estimate[sd]?|about)[[:space:]]+(?:of[[:space:]]+)?(?:about[[:space:]]+)?([0-9][0-9,]*)[[:space:]]*lines?"),
}

// sizeEstimateStated is the single largest estimate the text states, or 0 when it states none.
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
// sections as full text, so a prose estimate is read too. A field the input gives replaces the
// body's text for that field.
func sizeEstimateText(issue sizeIssue) string {
	parts := append(append([]string(nil), issue.scope...), issue.deliverablesText...)
	return strings.Join(parts, "\n")
}

// sizeEstimateSignal reads the body's stated estimate and scales it by the table's ratios; it
// answers nil when the body states none, so such an issue keeps the answer it had. overCeiling is
// the exact comparison stated x p75 > ceiling.
func sizeEstimateSignal(issue sizeIssue, ceiling int64) (*sizeEstimateReport, bool, error) {
	stated := sizeEstimateStated(sizeEstimateText(issue))
	if stated <= 0 {
		return nil, false, nil
	}
	rows, err := sizeEstimateReadCalibration(sizeEstimateCalibration)
	if err != nil {
		return nil, false, fmt.Errorf("the calibration table: %w", err)
	}
	dist := sizeEstimateDistributionOf(rows)
	scaled := new(big.Rat).Mul(new(big.Rat).SetInt64(stated), dist.p75)
	return &sizeEstimateReport{stated, sizeEstimateFloat(dist.p50), sizeEstimateFloat(dist.p75), sizeEstimateFloat(scaled), ceiling, len(rows)},
		scaled.Cmp(new(big.Rat).SetInt64(ceiling)) > 0, nil
}

// sizeEstimateReason is the reason a scaled estimate over the ceiling adds.
func sizeEstimateReason(e *sizeEstimateReport) string {
	return fmt.Sprintf("estimate_scaled_over_ceiling: the stated estimate %d lines scaled by the p75 ratio %v is %v, over the ceiling %d", e.Stated, e.P75Ratio, e.ScaledP75, e.Ceiling)
}

// sizeEstimateCeiling is the ceiling the input states, or the default when the field is absent; a
// negative ceiling is refused, since it would flag every body that states an estimate.
func sizeEstimateCeiling(stated *int) (int64, error) {
	switch {
	case stated == nil:
		return sizeEstimateDefaultCeiling, nil
	case *stated < 0:
		return 0, fmt.Errorf("size_ceiling %d is negative", *stated)
	}
	return int64(*stated), nil
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
	out := sizeEstimateCalibrateReport{sizeEstimateCalibrateSchema, len(rows), make([]sizeEstimateCalibrateRatio, 0, len(rows)), sizeEstimateFloat(dist.p50), sizeEstimateFloat(dist.p75)}
	for _, row := range rows {
		out.Ratios = append(out.Ratios, sizeEstimateCalibrateRatio{row.issue, row.estimate, row.actual, sizeEstimateFloat(row.value)})
	}
	_ = emit(stdout, out)
	return 0
}
