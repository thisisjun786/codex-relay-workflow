package interview

// The freeze manifest of CXC v0.2.40 (pabcd-state freeze.ts, commit 3c1459ac): the hashing and shaping behind crw pabcd freeze, ported
// as-is except for the order of plan file names with non-ASCII characters (localeCompare).

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

// The directories and file of the manifest under .crw, and of the plan under .crw/plan.
const (
	PlanSubdir         = "plan"
	FreezeManifestDir  = "interview"
	FreezeManifestFile = "freeze.json"
)

// PlanFileHash is one plan file: its path relative to the plan slug directory and the sha256 of its content.
type PlanFileHash struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// EvidenceBundle is the structured interview evidence carried into the goal handoff.
type EvidenceBundle struct {
	Dimensions         *Dimensions     `json:"dimensions"`
	OpenAssumptions    []string        `json:"openAssumptions"`
	Contradictions     []Contradiction `json:"contradictions"`
	AcceptanceCriteria []string        `json:"acceptanceCriteria"`
	ResearchReportRef  *string         `json:"researchReportRef"`
}

// FreezeManifest is the content of .crw/interview/freeze.json; PlanHash is the sha256 of the files' hashes in path order.
type FreezeManifest struct {
	FrozenAt       string         `json:"frozenAt"`
	PlanFiles      []PlanFileHash `json:"planFiles"`
	PlanHash       string         `json:"planHash"`
	Objective      string         `json:"objective"`
	Slug           string         `json:"slug"`
	EvidenceBundle EvidenceBundle `json:"evidenceBundle"`
}

// Sha256 is the hex sha256 of content's UTF-8 text.
func Sha256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// collationOrder lists the ASCII characters that carry a primary weight in ICU's root collation, lowest first, as Node's localeCompare
// orders them (recorded in testdata/oracle-freeze.json); a letter's two cases share one weight and the other ASCII characters, the
// controls, are ignorable.
const collationOrder = "\t\n\v\f\r _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$0123456789abcdefghijklmnopqrstuvwxyz"

// localeCompare compares the whole primary sequence first and then the whole case sequence (lower before upper). A character beyond
// ASCII weighs more than every ASCII one, by code point: ICU orders accents and scripts by tables this port does not have, so names
// with such characters can sort differently than they do under Node (known defect, port: pending).
func localeCompare(a, b string) int {
	weights := func(s string) (primary, upper []int) {
		for _, r := range s {
			w := 1000 + int(r)
			if r < 128 {
				if w = strings.IndexRune(collationOrder, unicode.ToLower(r)); w < 0 {
					continue
				}
			}
			up := 0
			if unicode.IsUpper(r) {
				up = 1
			}
			primary, upper = append(primary, w), append(upper, up)
		}
		return primary, upper
	}
	pa, ua := weights(a)
	pb, ub := weights(b)
	return cmp.Or(slices.Compare(pa, pb), slices.Compare(ua, ub))
}

func sortedByPath(files []PlanFileHash) []PlanFileHash {
	sorted := append([]PlanFileHash{}, files...)
	slices.SortStableFunc(sorted, func(a, b PlanFileHash) int { return localeCompare(a.Path, b.Path) })
	return sorted
}

// ComputePlanHash is sha256 of the files' hashes joined in path order.
func ComputePlanHash(files []PlanFileHash) string {
	var joined strings.Builder
	for _, f := range sortedByPath(files) {
		joined.WriteString(f.Sha256)
	}
	return Sha256(joined.String())
}

// DeriveSlug lowercases the objective, turns each run of characters outside [a-z0-9] into one dash, trims dashes, caps the slug at 48
// and trims again; nothing left is "interview".
func DeriveSlug(objective string) string {
	var slug []byte
	for _, r := range objective {
		if r == 0x130 { // JavaScript lowercases U+0130 to an i and a combining dot, Go to an i
			slug, r = append(slug, 'i'), 0x307
		}
		if r = unicode.ToLower(r); r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			slug = append(slug, byte(r))
		} else if len(slug) > 0 && slug[len(slug)-1] != '-' {
			slug = append(slug, '-')
		}
	}
	slug = slug[:min(len(slug), 48)]
	return cmp.Or(strings.TrimRight(string(slug), "-"), "interview")
}

// BuildManifestInput is what BuildFreezeManifest shapes into a manifest; Now defaults to the current time.
type BuildManifestInput struct {
	Objective      string
	PlanFiles      []PlanFileHash
	EvidenceBundle EvidenceBundle
	Now            func() string
}

// BuildFreezeManifest sorts a copy of the plan files, hashes them and derives the slug.
func BuildFreezeManifest(in BuildManifestInput) FreezeManifest {
	now := in.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	files := sortedByPath(in.PlanFiles)
	return FreezeManifest{FrozenAt: now(), PlanFiles: files, PlanHash: ComputePlanHash(files), Objective: in.Objective, Slug: DeriveSlug(in.Objective), EvidenceBundle: in.EvidenceBundle}
}

// StaleCheckResult is the verdict of CheckStale; ChangedFiles holds the changed paths in UTF-16 order, as JavaScript's default sort
// puts them (a path that is not a string, which only a hand-edited manifest has, is counted in Reason and not listed).
type StaleCheckResult struct {
	Stale        bool     `json:"stale"`
	ChangedFiles []string `json:"changedFiles"`
	Reason       string   `json:"reason"`
}

// jsVal is a JavaScript value as checkStale meets it in a manifest read back from a file, where a Map key or a hash compared with ===
// can be anything JSON holds. An object or array equals only itself (id); bad marks one that String() cannot convert: an object with
// an own toString member, which JSON makes non-callable, or an array holding one.
type jsVal struct {
	kind byte // u undefined, z null, b boolean, n number, s string, o object or array
	s    string
	n    float64
	id   int
	bad  bool
}

func jsString(s string) jsVal { return jsVal{kind: 's', s: s} }

type frozenEntry struct{ key, hash jsVal }

// staleCheck is checkStale: a path is changed when the current plan does not hold its frozen hash (strict equality, a missing
// path and a missing hash being both undefined) or is not frozen; stale is any change or a plan hash that is not the current one.
// frozen is a Map, so the last entry of a key wins. It fails where the oracle throws: the default sort of changedFiles converts every
// element but undefined to a string, which a bad value cannot do.
func staleCheck(frozen []frozenEntry, planHash jsVal, current []PlanFileHash) (StaleCheckResult, error) {
	entries, now := map[jsVal]jsVal{}, map[string]string{}
	for _, e := range frozen {
		entries[e.key] = e.hash
	}
	for _, f := range current {
		now[f.Path] = f.Sha256
	}
	var changed []jsVal
	for key, hash := range entries {
		got := jsVal{kind: 'u'}
		if v, ok := now[key.s]; ok && key.kind == 's' {
			got = jsString(v)
		}
		if got != hash {
			changed = append(changed, key)
		}
	}
	for path := range now {
		if _, ok := entries[jsString(path)]; !ok {
			changed = append(changed, jsString(path))
		}
	}
	listed, converted, bad := []string{}, 0, false
	for _, key := range changed {
		if key.kind == 's' {
			listed = append(listed, key.s)
		}
		if key.kind != 'u' {
			converted++
		}
		bad = bad || key.bad
	}
	if converted > 1 && bad {
		return StaleCheckResult{}, errors.New("cannot convert object to primitive value")
	}
	slices.SortFunc(listed, func(a, b string) int { return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b))) })
	hashChanged := planHash != jsString(ComputePlanHash(current))
	if len(changed) == 0 && !hashChanged {
		return StaleCheckResult{ChangedFiles: listed, Reason: "frozen manifest matches current plan"}, nil
	}
	hash := "matches"
	if hashChanged {
		hash = "differs"
	}
	return StaleCheckResult{true, listed, fmt.Sprintf("plan changed since freeze (%d file(s), planHash %s); re-freeze before goal start \u2014 stale execution refused", len(changed), hash)}, nil
}

// CheckStale compares a manifest with the plan as it is now.
func CheckStale(manifest FreezeManifest, current []PlanFileHash) StaleCheckResult {
	frozen := make([]frozenEntry, len(manifest.PlanFiles))
	for i, f := range manifest.PlanFiles {
		frozen[i] = frozenEntry{jsString(f.Path), jsString(f.Sha256)}
	}
	result, _ := staleCheck(frozen, jsString(manifest.PlanHash), current) // string paths never fail to convert
	return result
}

// GoalActivationDirective is GOAL_ACTIVATION_DIRECTIVE under CRW's names: plugin code cannot call create_goal, so freeze tells the
// main session to.
const GoalActivationDirective = "[crw: FREEZE -> goal handoff]\n" +
	"Interview is ready and the plan is frozen. To start execution under a native goal:\n" +
	"1. Call get_goal to confirm no goal is already active for this thread.\n" +
	"2. Call create_goal with objective ONLY (no token_budget \u2014 the L3 gate denies budgeted goals).\n" +
	"3. Verify a goal row was actually created (codex owns goal lifecycle in goals_1.sqlite).\n" +
	"The frozen plan under .crw/plan/ is the READ-ONLY spec the goal consumes; do not reopen\n" +
	"Interview once the goal is active (L11 hard-deny). If create_goal fails, report that goal mode\n" +
	"did not start \u2014 do not proceed as if it did. On goal start, recompute planHash and compare to\n" +
	".crw/interview/freeze.json; on mismatch, re-freeze the current plan before proceeding."
