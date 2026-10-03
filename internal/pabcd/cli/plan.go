// Package cli holds PABCD command libraries, separate from their harness routing.
package cli

import (
	"errors"
	"os"
	"time"
)

type PlanCliArgs struct {
	Verb   string  `json:"verb"`
	Slug   string  `json:"slug"`
	Phases int     `json:"phases"`
	Cwd    string  `json:"cwd"`
	Date   *string `json:"date"`
}
type PlanCliResult struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
}

func YYMMDD(t time.Time) string                    { return "" }
func SplitDatePrefix(raw string) (*string, string) { return nil, raw }
func DerivePlanSlug(raw string) string             { return "" }
func ParsePlanCliArgs(argv []string, cwd string) (PlanCliArgs, error) {
	return PlanCliArgs{}, errors.New("not implemented")
}
func RunPlanCli(args PlanCliArgs) PlanCliResult { return runPlanCli(args, writePlanDoc) }
func runPlanCli(args PlanCliArgs, writeDoc func(*os.Root, string, string) error) PlanCliResult {
	return PlanCliResult{Code: 1, Output: "not implemented"}
}
func writePlanDoc(root *os.Root, name, data string) error { return errors.New("not implemented") }
func planDoc(slug string) string                          { return "" }
func phaseDoc(n int, slug string) string                  { return "" }
