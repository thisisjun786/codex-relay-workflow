// Package risktier derives the risk tier of an issue branch head and records the decision (CRW-1041, part
// a). The tier says how much gating a change needs: a change is low only when every path it touches is on
// a known low-risk allowlist, and everything else is high. The tier of a head is the larger of the author's
// declaration and the tier derived from the declared edit regions and the actual changed paths, so an
// author can only raise it. The decision is written as a policy-decision/1 record
// (contract/schema/policy-decision.schema.json); it is not a pre-merge evaluation, carries no grader or
// score, and changes no gate by itself.
package risktier

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The tags a record carries.
const (
	RecordSchema = "policy-decision/1"
	PolicyName   = "risk-tier/1"
)

const instantLayout = "2006-01-02T15:04:05Z"

// Gates a record can never list as skipped: every node still produces its pre-merge record and its accept.
var neverSkipped = map[string]bool{"premerge_record": true, "dag_accept": true}

var (
	hex40    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	gateName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Change is one changed path of the head's diff against the dev commit it is judged on.
type Change struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"` // the source of a rename or a copy, and only of those
	Change  string `json:"change"`             // add, modify, delete, rename, copy or type_change
}

// Exception is the management session's recorded exception. It changes the tier whose gates run
// (ExecutionTier) and never the classification: the record keeps the tier the rules gave.
type Exception struct {
	By            string
	Reason        string
	At            time.Time
	ExecutionTier Tier
}

// SkippedGate is a gate that was really skipped for the head, with the reason.
type SkippedGate struct {
	Gate   string `json:"gate"`
	Reason string `json:"reason"`
}

// Input is everything a decision is made from.
type Input struct {
	Issue, Node, Head, Dev string
	DecidedAt              time.Time
	DecidedBy              string
	Declared               Tier // the author's declaration; empty when the issue declares none
	Regions                []Region
	Changes                []Change
	Exception              *Exception
	SkippedGates           []SkippedGate
}

// RegionEntry is a declared region with its verdict.
type RegionEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Key    string `json:"key,omitempty"`
	Change string `json:"change"`
	Tier   Tier   `json:"tier"`
	Rule   string `json:"rule"`
}

// ChangedEntry is a changed path with its verdict. For a rename or a copy the verdict is the higher of the
// two paths'.
type ChangedEntry struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Change  string `json:"change"`
	Tier    Tier   `json:"tier"`
	Rule    string `json:"rule"`
}

// ExceptionEntry is the exception as the record keeps it.
type ExceptionEntry struct {
	By            string `json:"by"`
	Reason        string `json:"reason"`
	At            string `json:"at"`
	ExecutionTier Tier   `json:"execution_tier"`
}

// Record is a policy-decision/1 document.
type Record struct {
	Schema          string          `json:"schema"`
	Policy          string          `json:"policy"`
	Issue           string          `json:"issue"`
	Node            string          `json:"node"`
	Head            string          `json:"head"`
	Dev             string          `json:"dev"`
	DecidedAt       string          `json:"decided_at"`
	DecidedBy       string          `json:"decided_by"`
	DeclaredTier    *Tier           `json:"declared_tier"`
	RegionTier      *Tier           `json:"region_tier"`
	ChangedTier     Tier            `json:"changed_tier"`
	DerivedTier     Tier            `json:"derived_tier"`
	Tier            Tier            `json:"tier"`
	ExecutionTier   Tier            `json:"execution_tier"`
	DeclaredRegions []RegionEntry   `json:"declared_regions"`
	ChangedPaths    []ChangedEntry  `json:"changed_paths"`
	OutsideRegions  []string        `json:"outside_regions"`
	Reclassified    bool            `json:"reclassified"`
	Exception       *ExceptionEntry `json:"exception,omitempty"`
	SkippedGates    []SkippedGate   `json:"skipped_gates"`
}

func instant(t time.Time) string { return t.UTC().Format(instantLayout) }

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func tierPtr(t Tier) *Tier { return &t }

func normalRegion(r Region) (Region, error) {
	if r.Path == "" {
		return r, errors.New("a declared region has no path")
	}
	if r.Change == "" {
		r.Change = "edit"
	}
	switch {
	case r.Kind != "tree" && r.Kind != "file" && r.Kind != "symbol":
		return r, fmt.Errorf("the kind %q of region %q is tree, file or symbol", r.Kind, r.Path)
	case r.Change != "edit" && r.Change != "rename" && r.Change != "delete":
		return r, fmt.Errorf("the change %q of region %q is edit, rename or delete", r.Change, r.Path)
	case r.Kind == "symbol" && r.Key == "":
		return r, fmt.Errorf("the symbol region %q names no symbol", r.Path)
	case r.Kind != "symbol" && r.Key != "":
		return r, fmt.Errorf("only a symbol region has a key, not %q", r.Path)
	}
	return r, nil
}

func validChange(c Change) error {
	if c.Path == "" {
		return errors.New("a changed path is empty")
	}
	switch c.Change {
	case "add", "modify", "delete", "type_change":
		if c.OldPath != "" {
			return fmt.Errorf("the %s of %q has an old path; only a rename or a copy has one", c.Change, c.Path)
		}
	case "rename", "copy":
		if c.OldPath == "" {
			return fmt.Errorf("the %s of %q names no old path", c.Change, c.Path)
		}
	default:
		return fmt.Errorf("the change %q of %q is add, modify, delete, rename, copy or type_change", c.Change, c.Path)
	}
	return nil
}

func changedVerdict(c Change) Verdict {
	if c.Change == "type_change" {
		return Verdict{High, RuleTypeChange}
	}
	v := Classify(c.Path)
	if c.OldPath != "" && v.Tier == Low {
		if old := Classify(c.OldPath); old.Tier == High {
			return old
		}
	}
	return v
}

// Decide derives the decision record for one head. It refuses an input that does not describe a head
// (a missing identity, a malformed commit, an unknown change kind) and never refuses a path: a path it
// cannot place is high.
func Decide(in Input) (Record, error) {
	for name, v := range map[string]string{"issue": in.Issue, "node": in.Node, "decided_by": in.DecidedBy} {
		if blank(v) {
			return Record{}, fmt.Errorf("the %s is empty", name)
		}
	}
	if !hex40.MatchString(in.Head) || !hex40.MatchString(in.Dev) {
		return Record{}, errors.New("head and dev are 40 lower-case hex digits")
	}
	if in.DecidedAt.IsZero() {
		return Record{}, errors.New("the decision has no time")
	}
	if in.Declared != "" && !in.Declared.valid() {
		return Record{}, fmt.Errorf("the declared tier %q is low or high", in.Declared)
	}

	regions := make([]RegionEntry, 0, len(in.Regions))
	var regionList []Region
	var regionTier *Tier
	for _, r := range in.Regions {
		r, err := normalRegion(r)
		if err != nil {
			return Record{}, err
		}
		v := ClassifyRegion(r)
		regions = append(regions, RegionEntry{Path: r.Path, Kind: r.Kind, Key: r.Key, Change: r.Change, Tier: v.Tier, Rule: v.Rule})
		regionList = append(regionList, r)
		t := v.Tier
		if regionTier != nil {
			t = Max(*regionTier, t)
		}
		regionTier = tierPtr(t)
	}
	sort.SliceStable(regions, func(i, j int) bool {
		a, b := regions[i], regions[j]
		return lessStrings([]string{a.Path, a.Kind, a.Key, a.Change}, []string{b.Path, b.Kind, b.Key, b.Change})
	})

	changed := make([]ChangedEntry, 0, len(in.Changes))
	changedTier := High // an empty diff establishes nothing low
	if len(in.Changes) > 0 {
		changedTier = Low
	}
	outside := map[string]bool{}
	for _, c := range in.Changes {
		if err := validChange(c); err != nil {
			return Record{}, err
		}
		v := changedVerdict(c)
		changed = append(changed, ChangedEntry{Path: c.Path, OldPath: c.OldPath, Change: c.Change, Tier: v.Tier, Rule: v.Rule})
		changedTier = Max(changedTier, v.Tier)
		if len(regionList) > 0 {
			for _, p := range []string{c.Path, c.OldPath} {
				if p != "" && !coveredByAny(regionList, p) {
					outside[p] = true
				}
			}
		}
	}
	sort.SliceStable(changed, func(i, j int) bool {
		a, b := changed[i], changed[j]
		return lessStrings([]string{a.Path, a.OldPath, a.Change}, []string{b.Path, b.OldPath, b.Change})
	})
	outsideList := make([]string, 0, len(outside))
	for p := range outside {
		outsideList = append(outsideList, p)
	}
	sort.Strings(outsideList)

	derived := changedTier
	if regionTier != nil {
		derived = Max(derived, *regionTier)
	}
	tier := derived
	var declared *Tier
	if in.Declared != "" {
		declared = tierPtr(in.Declared)
		tier = Max(tier, in.Declared)
	}

	execution := tier
	var exception *ExceptionEntry
	if e := in.Exception; e != nil {
		switch {
		case blank(e.By), blank(e.Reason):
			return Record{}, errors.New("an exception names who recorded it and why")
		case e.At.IsZero():
			return Record{}, errors.New("an exception has a time")
		case !e.ExecutionTier.valid():
			return Record{}, fmt.Errorf("the exception's execution tier %q is low or high", e.ExecutionTier)
		case e.ExecutionTier == tier:
			return Record{}, fmt.Errorf("an exception to %s is no exception: the tier already is %s", e.ExecutionTier, tier)
		}
		execution = e.ExecutionTier
		exception = &ExceptionEntry{By: e.By, Reason: e.Reason, At: instant(e.At), ExecutionTier: e.ExecutionTier}
	}

	skipped := make([]SkippedGate, 0, len(in.SkippedGates))
	seen := map[string]bool{}
	for _, g := range in.SkippedGates {
		switch {
		case !gateName.MatchString(g.Gate):
			return Record{}, fmt.Errorf("the skipped gate %q is not a gate name (lower-case letters, digits, underscores)", g.Gate)
		case neverSkipped[g.Gate]:
			return Record{}, fmt.Errorf("the gate %s is never skipped: every node still produces it", g.Gate)
		case blank(g.Reason):
			return Record{}, fmt.Errorf("the skipped gate %s has no reason", g.Gate)
		case seen[g.Gate]:
			return Record{}, fmt.Errorf("the gate %s is listed twice", g.Gate)
		case execution == High:
			return Record{}, fmt.Errorf("the gate %s is skipped at execution tier high; a high tier keeps every gate", g.Gate)
		}
		seen[g.Gate] = true
		skipped = append(skipped, g)
	}
	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].Gate < skipped[j].Gate })

	return Record{
		Schema: RecordSchema, Policy: PolicyName,
		Issue: in.Issue, Node: in.Node, Head: in.Head, Dev: in.Dev,
		DecidedAt: instant(in.DecidedAt), DecidedBy: in.DecidedBy,
		DeclaredTier: declared, RegionTier: regionTier, ChangedTier: changedTier, DerivedTier: derived, Tier: tier, ExecutionTier: execution,
		DeclaredRegions: regions, ChangedPaths: changed, OutsideRegions: outsideList,
		Reclassified: regionTier != nil && len(outsideList) > 0 && derived != *regionTier,
		Exception:    exception, SkippedGates: skipped,
	}, nil
}

func coveredByAny(regions []Region, p string) bool {
	for _, r := range regions {
		if r.covers(p) {
			return true
		}
	}
	return false
}

func lessStrings(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// inputOf is the input a record was made from, read back from its own members.
func inputOf(r Record) (Input, error) {
	at, err := time.Parse(time.RFC3339, r.DecidedAt)
	if err != nil {
		return Input{}, fmt.Errorf("decided_at is not a time: %w", err)
	}
	in := Input{Issue: r.Issue, Node: r.Node, Head: r.Head, Dev: r.Dev, DecidedAt: at, DecidedBy: r.DecidedBy, SkippedGates: r.SkippedGates}
	if r.DeclaredTier != nil {
		in.Declared = *r.DeclaredTier
		if !in.Declared.valid() {
			return Input{}, fmt.Errorf("declared_tier %q is low, high or null", in.Declared)
		}
	}
	for _, e := range r.DeclaredRegions {
		in.Regions = append(in.Regions, Region{Path: e.Path, Kind: e.Kind, Key: e.Key, Change: e.Change})
	}
	for _, e := range r.ChangedPaths {
		in.Changes = append(in.Changes, Change{Path: e.Path, OldPath: e.OldPath, Change: e.Change})
	}
	if e := r.Exception; e != nil {
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil {
			return Input{}, fmt.Errorf("the exception's time is not a time: %w", err)
		}
		in.Exception = &Exception{By: e.By, Reason: e.Reason, At: at, ExecutionTier: e.ExecutionTier}
	}
	return in, nil
}

// requiredMembers are the members every record has; the exception is the only one that may be absent.
var requiredMembers = []string{"schema", "policy", "issue", "node", "head", "dev", "decided_at", "decided_by", "declared_tier", "region_tier",
	"changed_tier", "derived_tier", "tier", "execution_tier", "declared_regions", "changed_paths", "outside_regions", "reclassified", "skipped_gates"}

// Decode reads a policy-decision/1 document and judges it. A record is accepted only when it is exactly
// the record Decide gives for the inputs it carries: the tier, every verdict, the outside list, the
// execution tier and the skipped gates are recomputed, so a record cannot state a lower tier than its own
// paths give, or skip a gate under a high execution tier.
func Decode(data []byte) (Record, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return Record{}, fmt.Errorf("a policy-decision record is a JSON object: %w", err)
	}
	for _, m := range requiredMembers {
		if _, ok := members[m]; !ok {
			return Record{}, fmt.Errorf("the record lacks %s", m)
		}
	}
	if err := refuseNulls(members); err != nil {
		return Record{}, err
	}
	var r Record
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("the record does not read as %s: %w", RecordSchema, err)
	}
	if decoder.More() {
		return Record{}, errors.New("the record is followed by more data")
	}
	if err := sameSpelling(data, r); err != nil {
		return Record{}, err
	}
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// sameSpelling refuses a document that says more or other than the record read from it: encoding/json
// matches member names without regard to case (so "TIER" or "PATH" would be read as tier or path, which
// the schema's additionalProperties and required refuse) and omitempty drops an empty optional member
// (an "old_path" or a "key" of "", which the schema's minLength refuses). The document, read as plain
// JSON, must equal the record written back, member for member.
func sameSpelling(data []byte, r Record) error {
	var doc, back any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("a policy-decision record is a JSON object: %w", err)
	}
	written, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(written, &back); err != nil {
		return err
	}
	if where := firstDifference(doc, back, ""); where != "" {
		return fmt.Errorf("the record's %s is not spelled as %s spells it (a member name differs in case, is unknown, or is an empty optional member)", where, RecordSchema)
	}
	return nil
}

// firstDifference is the JSON pointer of the first place where two plain JSON values differ, or "" when
// they are equal.
func firstDifference(a, b any, at string) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return pointer(at)
		}
		names := make([]string, 0, len(x)+len(y))
		for k := range x {
			names = append(names, k)
		}
		for k := range y {
			if _, ok := x[k]; !ok {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		for _, k := range names {
			xv, inX := x[k]
			yv, inY := y[k]
			if !inX || !inY {
				return pointer(at + "/" + k)
			}
			if d := firstDifference(xv, yv, at+"/"+k); d != "" {
				return d
			}
		}
		return ""
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return pointer(at)
		}
		for i := range x {
			if d := firstDifference(x[i], y[i], fmt.Sprintf("%s/%d", at, i)); d != "" {
				return d
			}
		}
		return ""
	default:
		if a != b {
			return pointer(at)
		}
		return ""
	}
}

func pointer(at string) string {
	if at == "" {
		return "document"
	}
	return "member " + at
}

// nullableMembers are the only members the schema lets be null; a null anywhere else is a type violation
// that decoding into Go values would otherwise erase into false, nil or an empty string.
var nullableMembers = map[string]bool{"declared_tier": true, "region_tier": true}

func refuseNulls(members map[string]json.RawMessage) error {
	for name, raw := range members {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("the member %s is not JSON: %w", name, err)
		}
		if nullableMembers[name] && v == nil {
			continue
		}
		if hasNull(v) {
			return fmt.Errorf("the member %s holds a null; only declared_tier and region_tier may be null", name)
		}
	}
	return nil
}

func hasNull(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case []any:
		for _, e := range x {
			if hasNull(e) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if hasNull(e) {
				return true
			}
		}
	}
	return false
}

// Validate judges a record against the decision its own inputs give.
func (r Record) Validate() error {
	in, err := inputOf(r)
	if err != nil {
		return err
	}
	want, err := Decide(in)
	if err != nil {
		return err
	}
	got, _ := json.Marshal(r)
	exp, _ := json.Marshal(want)
	if bytes.Equal(got, exp) {
		return nil
	}
	var g, w map[string]json.RawMessage
	_ = json.Unmarshal(got, &g)
	_ = json.Unmarshal(exp, &w)
	for _, m := range append(append([]string{}, requiredMembers...), "exception") {
		if !bytes.Equal(g[m], w[m]) {
			return fmt.Errorf("the record's %s is %s; its own inputs give %s", m, clip(g[m]), clip(w[m]))
		}
	}
	return errors.New("the record is not the decision its inputs give")
}

func clip(b json.RawMessage) string {
	if len(b) == 0 {
		return "absent"
	}
	if len(b) > 160 {
		return string(b[:160]) + "..."
	}
	return string(b)
}
