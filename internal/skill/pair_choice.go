package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const pairInputLimit = 1 << 20
const pairWindowSeconds int64 = 5 * 60 * 60
const pairHysteresisPoints = 10
const pairRatioPercent = 60
const pairQuotaMaxAge = 30 * time.Minute

type pairCounts struct {
	Sonnet int `json:"sonnet"`
	SOL    int `json:"sol"`
}

func (c pairCounts) count(p string) int {
	if p == "Sonnet" {
		return c.Sonnet
	}
	return c.SOL
}
func (c *pairCounts) add(p string) {
	if p == "Sonnet" {
		c.Sonnet++
	} else {
		c.SOL++
	}
}

type pairCauses struct {
	Sonnet string `json:"sonnet"`
	SOL    string `json:"sol"`
}

func (c pairCauses) cause(p string) string {
	if p == "Sonnet" {
		return c.Sonnet
	}
	return c.SOL
}

type pairRelease struct {
	At     time.Time `json:"at"`
	Pair   string    `json:"pair"`
	Bundle string    `json:"bundle"`
}
type pairRequest struct {
	Bundle               string        `json:"bundle"`
	AsOf                 time.Time     `json:"as_of"`
	Working              pairCounts    `json:"working"`
	Lines                pairCounts    `json:"lines"`
	Tie                  string        `json:"tie"`
	Recorded             string        `json:"recorded_pair"`
	Source               string        `json:"source"`
	ClassificationReason string        `json:"classification_reason"`
	Unusable             pairCauses    `json:"unusable"`
	Releases             []pairRelease `json:"releases"`
}
type pairWindow struct {
	Start  time.Time  `json:"start"`
	End    time.Time  `json:"end"`
	Counts pairCounts `json:"counts"`
}
type pairReport struct {
	Schema               string         `json:"schema"`
	Bundle               string         `json:"bundle"`
	Pair                 *string        `json:"pair"`
	Source               string         `json:"source"`
	Rule                 []string       `json:"rule"`
	DefaultPair          string         `json:"default_pair"`
	Quota                pairQuota      `json:"quota"`
	Window               pairWindow     `json:"window"`
	Cause                string         `json:"cause,omitempty"`
	Date                 string         `json:"date,omitempty"`
	ClassificationReason string         `json:"classification_reason,omitempty"`
	Limits               map[string]int `json:"limits"`
}

func pairName(p string) bool { return p == "Sonnet" || p == "SOL" }
func otherPair(p string) string {
	if p == "Sonnet" {
		return "SOL"
	}
	return "Sonnet"
}
func pairBundle(b string) bool {
	return b == "flexible" || b == "Sonnet fixed" || b == "SOL fixed" || b == "undetermined"
}

// Only explicitly supplied files are opened; there is no default operating-data path.
func pairInput(path string, stdin io.Reader) ([]byte, error) {
	if path != "" {
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		stdin = f
	}
	b, e := io.ReadAll(io.LimitReader(stdin, pairInputLimit+1))
	if e == nil && len(b) > pairInputLimit {
		e = errors.New("input too large")
	}
	return b, e
}
func pairJSON(b []byte, dst any) error {
	if !utf8.Valid(b) {
		return errNotUTF8
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(dst); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}
func validatePairRequest(r *pairRequest) error {
	if r == nil || !pairBundle(r.Bundle) || r.AsOf.IsZero() || r.AsOf.Unix() < 0 {
		return errors.New("bundle and as_of are required")
	}
	if r.Tie == "" {
		r.Tie = "Sonnet"
	}
	if !pairName(r.Tie) {
		return errors.New("invalid tie")
	}
	for _, n := range []int{r.Working.Sonnet, r.Working.SOL, r.Lines.Sonnet, r.Lines.SOL} {
		if n < 0 || n > 1000000 {
			return errors.New("counts must be between 0 and 1000000")
		}
	}
	if r.Recorded != "" {
		if !pairName(r.Recorded) {
			return errors.New("invalid recorded_pair")
		}
		if r.Source == "" {
			r.Source = "issue body"
		}
	}
	if r.Source != "" {
		if r.Recorded == "" || (r.Source != "table" && r.Source != "quota" && r.Source != "issue body" && r.Source != "user choice") {
			return errors.New("invalid recorded source")
		}
	}
	if r.Bundle == "undetermined" && (r.Recorded == "" || (r.Source != "issue body" && r.Source != "user choice") || strings.TrimSpace(r.ClassificationReason) == "") {
		return errors.New("undetermined requires a preserved pair and classification_reason")
	}
	for _, v := range r.Releases {
		if !pairName(v.Pair) || !pairBundle(v.Bundle) || v.At.IsZero() || v.At.After(r.AsOf) {
			return errors.New("invalid release history")
		}
	}
	return nil
}
func runPairChoice(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	name, code, ok := pairChoice.command(args, stdout, stderr)
	if !ok {
		return code
	}
	flags := newCommandLine("pair-choice", name, "Choose a pair from a bundle, release counts and an optional CRW-owned quota snapshot. No live quota source is read.").takes("request", 0, 1)
	snapshot := flags.String("snapshot", "", "optional crw-pair-quota/1 JSON file; absent or unreadable uses the table")
	pos, code := flags.parse(args[1:], stdout, stderr)
	if code >= 0 {
		return code
	}
	path := ""
	if len(pos) == 1 {
		path = pos[0]
	}
	b, e := pairInput(path, stdin)
	if e != nil {
		fmt.Fprintln(stderr, "Pair choice request could not be read.")
		return 3
	}
	var r *pairRequest
	if e = pairJSON(b, &r); e == nil {
		e = validatePairRequest(r)
	}
	if e != nil {
		fmt.Fprintln(stderr, "Unreadable pair choice request:", e)
		return 2
	}
	result := choosePair(*r, readPairQuota(*snapshot, r.AsOf))
	if e = emit(stdout, result); e != nil {
		fmt.Fprintln(stderr, "Pair choice output could not be written.")
		return 3
	}
	if result.Pair == nil {
		return 1
	}
	return 0
}
