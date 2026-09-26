package delivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Criteria refusals and modes (criteria.py, errors.RefusalReason).
const (
	CriteriaUnregistered = "criteria_unregistered"
	CriteriaNotCovered   = "criteria_not_covered"
	CriteriaSetChanged   = "criteria_set_changed"
	UnknownCriterion     = "unknown_criterion"
	FindingsRequired     = "findings_required"
	DispositionConflict  = "disposition_conflict"
	ReviewNotBound       = "review_not_bound"
	Managed              = "managed"
	Legacy               = "legacy"
	Covered              = "covered"
	LegacyUnregistered   = "legacy_unregistered"
)

var dispositions = []string{"verified", "needs_changes", "unverified"}

// Criterion is one canonical criterion.
type Criterion struct {
	ID, Title string
	Required  bool
}

func (c Criterion) obj() Obj {
	return Obj{{Key: "id", Value: c.ID}, {Key: "title", Value: c.Title}, {Key: "required", Value: c.Required}}
}

func criteriaObjs(cs []Criterion) []any {
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = c.obj()
	}
	return out
}

// jsonCompact is json.dumps(..., sort_keys=True, separators=(",", ":"), ensure_ascii=False).
func jsonCompact(v any) string {
	var b strings.Builder
	var write func(any)
	write = func(v any) {
		switch t := v.(type) {
		case Obj:
			sorted := append(Obj(nil), t...)
			sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
			b.WriteByte('{')
			for i, f := range sorted {
				if i > 0 {
					b.WriteByte(',')
				}
				writeRawString(&b, f.Key)
				b.WriteByte(':')
				write(f.Value)
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, x := range t {
				if i > 0 {
					b.WriteByte(',')
				}
				write(x)
			}
			b.WriteByte(']')
		case string:
			writeRawString(&b, t)
		default:
			b.WriteString(dumps(v))
		}
	}
	write(v)
	return b.String()
}

// writeRawString is a JSON string with ensure_ascii=False: only quote, backslash and controls escape.
func writeRawString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// SetDigest is criteria.set_digest: canonical JSON, so no delimiter can collide two sets.
func SetDigest(cs []Criterion) string {
	sorted := slices.Clone(cs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	sum := sha256.Sum256([]byte(jsonCompact(criteriaObjs(sorted))))
	return hex.EncodeToString(sum[:])
}

// NormaliseCriteria is normalise_criteria: the one shape a set is stored in.
func NormaliseCriteria(entries []any) ([]Criterion, error) {
	var out []Criterion
	seen := map[string]bool{}
	for _, entry := range entries {
		o, ok := entry.(Obj)
		if !ok {
			return nil, refuse(CriteriaUnregistered, "each criterion is an object with an id and a title")
		}
		idv, _ := get(o, "id")
		titlev, _ := get(o, "title")
		id := strings.TrimSpace(pyStrOrEmpty(idv))
		title := strings.TrimSpace(pyStrOrEmpty(titlev))
		if id == "" || title == "" {
			return nil, refuse(CriteriaUnregistered, "each criterion needs a non-empty id and title")
		}
		if seen[id] {
			return nil, refuse(CriteriaUnregistered, "duplicate criterion id %s", store.PyRepr(id))
		}
		seen[id] = true
		required := true
		if r, present := get(o, "required"); present {
			required = truthy(r)
		}
		out = append(out, Criterion{id, title, required})
	}
	if len(out) == 0 {
		return nil, refuse(CriteriaUnregistered, "a criteria set needs at least one criterion")
	}
	return out, nil
}

// pyStrOrEmpty is str(value or "").
func pyStrOrEmpty(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
}

// NormaliseFindings is normalise_findings: criteria and findings merged by id, dispositions in
// the frozen enum, at most one restoration carrier and never an empty one.
func NormaliseFindings(sources ...[]any) ([]any, error) {
	var merged []Obj
	declared := map[string]bool{}
	declaredSet := map[string]bool{}
	for _, source := range sources {
		for _, item := range source {
			o, ok := item.(Obj)
			if !ok {
				return nil, refuse(DispositionConflict, "each finding is an object")
			}
			idv, _ := get(o, "id")
			id := strings.TrimSpace(pyStrOrEmpty(idv))
			if id == "" {
				return nil, refuse(DispositionConflict, "each finding names a criterion id")
			}
			disposition := "verified"
			if v, _ := get(o, "verdict"); truthy(v) {
				disposition = pyStr(v)
				if _, isText := v.(string); !isText || !slices.Contains(dispositions, disposition) {
					return nil, refuse(DispositionConflict, "%s is not one of ('verified', 'needs_changes', 'unverified'); the contract's criteria enum is frozen and a finding outside it cannot be recorded", pyReprValue(v))
				}
			}
			entry := Obj{{Key: "id", Value: id}, {Key: "verdict", Value: disposition}}
			notev, _ := get(o, "note")
			if note := strings.TrimSpace(pyStrOrEmpty(notev)); note != "" {
				entry = append(entry, F{Key: "note", Value: note})
			}
			if flag, present := get(o, "restoration"); present && flag != nil {
				b, isBool := flag.(bool)
				if !isBool {
					return nil, refuse(DispositionConflict, "a finding declares its restoration block with true or false, not %s", pyTypeName(flag))
				}
				if declaredSet[id] && declared[id] != b {
					return nil, refuse(DispositionConflict, "%s both declares and disclaims the restoration block; one correction carries one block and says so once", store.PyRepr(id))
				}
				declared[id], declaredSet[id] = b, true
			}
			if declared[id] {
				entry = append(entry, F{Key: "restoration", Value: true})
			}
			merged = slices.DeleteFunc(merged, func(e Obj) bool { return str(e, "id") == id })
			merged = append(merged, entry)
		}
	}
	var carriers []string
	for _, e := range merged {
		if v, _ := get(e, "restoration"); v == true {
			carriers = append(carriers, str(e, "id"))
		}
	}
	if len(carriers) > 1 {
		return nil, refuse(DispositionConflict, "%s each declare the restoration block. One correction carries one block, and two candidates is a block nobody can locate", reprList(carriers))
	}
	for _, e := range merged {
		if v, _ := get(e, "restoration"); v == true {
			if _, hasNote := get(e, "note"); !hasNote {
				return nil, refuse(DispositionConflict, "%s declares the restoration block and carries no note. The block is the note; a declaration without one names a carrier with nothing in it", store.PyRepr(str(e, "id")))
			}
		}
	}
	out := make([]any, len(merged))
	for i, e := range merged {
		out[i] = e
	}
	return out, nil
}

// Criteria is criteria.CriteriaService.
type Criteria struct {
	Store *store.Store
	Clock Clock
}

func (c *Criteria) registration(rid string, cs []Criterion, digest string, source any) Obj {
	return Obj{{Key: "relationshipId", Value: rid}, {Key: "setDigest", Value: digest}, {Key: "sourceRef", Value: source}, {Key: "criteria", Value: criteriaObjs(cs)}, {Key: "mode", Value: Managed}}
}

// Register is register: an intentional replacement.
func (c *Criteria) Register(ctx context.Context, rid string, entries []any, source any) (Obj, error) {
	cs, err := NormaliseCriteria(entries)
	if err != nil {
		return nil, err
	}
	digest, now := SetDigest(cs), c.Clock.ISO()
	err = c.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return c.replace(ctx, rid, cs, digest, source, now) })
	if err != nil {
		return nil, err
	}
	return c.registration(rid, cs, digest, source), nil
}

// EnsureRegistered is ensure_registered: an exact replay is a no-op; a different set refuses.
func (c *Criteria) EnsureRegistered(ctx context.Context, rid string, entries []any, source any) (Obj, error) {
	cs, err := NormaliseCriteria(entries)
	if err != nil {
		return nil, err
	}
	digest, now := SetDigest(cs), c.Clock.ISO()
	err = c.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		existing, err := c.lockedSet(ctx, rid)
		if err != nil {
			return err
		}
		if existing == nil {
			return c.replace(ctx, rid, cs, digest, source, now)
		}
		sorted := slices.Clone(cs)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
		if !slices.Equal(existing.criteria, sorted) || existing.digest != digest || existing.source != source || existing.mode != Managed {
			return refuse(CriteriaSetChanged, "criteria for %s are already registered as %s; ensure_registered does not replace them", store.PyRepr(rid), existing.digest)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.registration(rid, cs, digest, source), nil
}

type storedSet struct {
	criteria []Criterion
	digest   string
	source   any
	mode     string
}

func (c *Criteria) lockedSet(ctx context.Context, rid string) (*storedSet, error) {
	rows, err := all(ctx, c.Store, "SELECT criterion_id, title, required, source_ref, set_digest, recorded_at FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id", rid)
	if err != nil {
		return nil, err
	}
	mode, err := one(ctx, c.Store, "SELECT mode FROM verification_mode WHERE relationship_id = ?", rid)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 && mode == nil {
		return nil, nil
	}
	var cs []Criterion
	sources, digests := map[any]bool{}, map[string]bool{}
	for _, row := range rows {
		if r := row.I("required"); r != 0 && r != 1 {
			return nil, refuse(CriteriaSetChanged, "stored required flag for %s is not 0 or 1", store.PyRepr(row.S("criterion_id")))
		}
		cs = append(cs, Criterion{row.S("criterion_id"), row.S("title"), row.I("required") == 1})
		sources[row.Opt("source_ref")] = true
		digests[row.S("set_digest")] = true
	}
	if len(cs) == 0 || mode == nil || mode.S("mode") != Managed || len(sources) != 1 || len(digests) != 1 || !digests[SetDigest(cs)] {
		return nil, refuse(CriteriaSetChanged, "criteria for %s are stored in a form ensure_registered will not replace or repair", store.PyRepr(rid))
	}
	return &storedSet{cs, rows[0].S("set_digest"), rows[0].Opt("source_ref"), mode.S("mode")}, nil
}

func (c *Criteria) replace(ctx context.Context, rid string, cs []Criterion, digest string, source any, now string) error {
	if _, err := execSQL(ctx, c.Store, "DELETE FROM canonical_criteria WHERE relationship_id = ?", rid); err != nil {
		return err
	}
	for _, e := range cs {
		required := int64(0)
		if e.Required {
			required = 1
		}
		if _, err := execSQL(ctx, c.Store, "INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, source_ref, set_digest, recorded_at) VALUES (?,?,?,?,?,?,?)", rid, e.ID, e.Title, required, source, digest, now); err != nil {
			return err
		}
	}
	if err := c.writeMode(ctx, rid, Managed, now); err != nil {
		return err
	}
	return journal(ctx, c.Store, "criteria_registered", rid, Obj{{Key: "setDigest", Value: digest}, {Key: "count", Value: int64(len(cs))}}, now)
}

func (c *Criteria) writeMode(ctx context.Context, rid, mode, now string) error {
	_, err := execSQL(ctx, c.Store, "INSERT INTO verification_mode (relationship_id, mode, recorded_at) VALUES (?,?,?) ON CONFLICT(relationship_id) DO UPDATE SET mode = excluded.mode, recorded_at = excluded.recorded_at", rid, mode, now)
	return err
}

// SetMode is set_mode.
func (c *Criteria) SetMode(ctx context.Context, rid, mode string) (Obj, error) {
	if mode != Managed && mode != Legacy {
		return nil, refuse(DispositionConflict, "unknown verification mode %s", store.PyRepr(mode))
	}
	err := c.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return c.writeMode(ctx, rid, mode, c.Clock.ISO()) })
	return Obj{{Key: "relationshipId", Value: rid}, {Key: "mode", Value: mode}}, err
}

// Get is get: the registered set, or nil.
func (c *Criteria) Get(ctx context.Context, rid string) (Obj, error) {
	rows, err := all(ctx, c.Store, "SELECT * FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id", rid)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var cs []any
	for _, r := range rows {
		cs = append(cs, Criterion{r.S("criterion_id"), r.S("title"), r.I("required") != 0}.obj())
	}
	return Obj{{Key: "relationshipId", Value: rid}, {Key: "setDigest", Value: rows[0].S("set_digest")}, {Key: "sourceRef", Value: rows[0].Opt("source_ref")}, {Key: "criteria", Value: cs}}, nil
}

// Mode is mode.
func (c *Criteria) Mode(ctx context.Context, rid string) (string, error) {
	row, err := one(ctx, c.Store, "SELECT mode FROM verification_mode WHERE relationship_id = ?", rid)
	if err != nil || row == nil {
		return Legacy, err
	}
	return row.S("mode"), nil
}

// BindReview is bind_review: the set's digest as it stands at review start.
func (c *Criteria) BindReview(ctx context.Context, rid, eventID string) error {
	current, err := c.Get(ctx, rid)
	if err != nil {
		return err
	}
	var digest any
	if current != nil {
		digest = str(current, "setDigest")
	}
	_, err = execSQL(ctx, c.Store, "INSERT OR IGNORE INTO claim_context (event_id, set_digest, bound_at) VALUES (?,?,?)", eventID, digest, c.Clock.ISO())
	return err
}

// BoundDigest is bound_digest.
func (c *Criteria) BoundDigest(ctx context.Context, eventID string) (any, error) {
	row, err := one(ctx, c.Store, "SELECT set_digest FROM claim_context WHERE event_id = ?", eventID)
	if err != nil || row == nil {
		return nil, err
	}
	return row.Opt("set_digest"), nil
}

// Coverage is coverage: the verdict's findings against the canonical set.
func (c *Criteria) Coverage(ctx context.Context, rid, eventID, verdict string, findings []any, reason any, expected any) (Obj, error) {
	registered, err := c.Get(ctx, rid)
	if err != nil {
		return nil, err
	}
	mode, err := c.Mode(ctx, rid)
	if err != nil {
		return nil, err
	}
	if findings == nil {
		findings = []any{}
	}
	if registered == nil {
		if mode == Managed {
			return nil, refuse(CriteriaUnregistered, "%s is a managed assignment with no canonical criteria; a managed assignment cannot be completed against nothing", store.PyRepr(rid))
		}
		return Obj{{Key: "coverage", Value: LegacyUnregistered}, {Key: "setDigest", Value: nil}, {Key: "boundDigest", Value: nil}, {Key: "findings", Value: findings}}, nil
	}
	digest := str(registered, "setDigest")
	bound, err := c.BoundDigest(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if mode == Managed && bound == nil && expected == nil && (verdict == "verified" || verdict == "needs_changes") {
		return nil, refuse(ReviewNotBound, "this managed review is not bound to a criteria set: claim the event first, or pass the reviewed digest explicitly. Current set is %s", digest)
	}
	if bound != nil && bound != digest {
		return nil, refuse(CriteriaSetChanged, "the criteria set changed after this review was claimed: bound %s, current %s. Findings made against the previous wording cannot certify the current one; claim the review again", pyStr(bound), digest)
	}
	if expected != nil && expected != digest {
		return nil, refuse(CriteriaSetChanged, "expected criteria set %s, but the current set is %s", pyStr(expected), digest)
	}
	list, _ := get(registered, "criteria")
	known, required := map[string]bool{}, []string{}
	for _, x := range list.([]any) {
		o := x.(Obj)
		known[str(o, "id")] = true
		if v, _ := get(o, "required"); v == true {
			required = append(required, str(o, "id"))
		}
	}
	byID := map[string]Obj{}
	for _, f := range findings {
		o := f.(Obj)
		if !known[str(o, "id")] {
			return nil, refuse(UnknownCriterion, "%s is not in this assignment's canonical criteria", store.PyRepr(str(o, "id")))
		}
		byID[str(o, "id")] = o
	}
	hasNote := func(o Obj) bool { v, _ := get(o, "note"); return truthy(v) }
	switch verdict {
	case "verified":
		var missing []string
		for _, id := range required {
			if o, ok := byID[id]; !ok || str(o, "verdict") != "verified" {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			return nil, refuse(CriteriaNotCovered, "these required criteria are not recorded as verified: %s", reprList(missing))
		}
	case "needs_changes":
		ok := false
		for _, o := range byID {
			if str(o, "verdict") == "needs_changes" && hasNote(o) {
				ok = true
			}
		}
		if !ok {
			return nil, refuse(FindingsRequired, "a needs_changes verdict needs at least one criterion marked needs_changes with a note; a correction with no findings is one nobody can act on")
		}
	case "unverified":
		ok := false
		for _, o := range byID {
			if str(o, "verdict") == "unverified" && hasNote(o) {
				ok = true
			}
		}
		if !ok {
			return nil, refuse(FindingsRequired, "an unverified verdict names at least one criterion it could not verify, with the reason")
		}
	case "aborted":
		if strings.TrimSpace(pyStrOrEmpty(reason)) == "" {
			return nil, refuse(FindingsRequired, "an aborted verdict states why")
		}
	}
	return Obj{{Key: "coverage", Value: Covered}, {Key: "setDigest", Value: digest}, {Key: "boundDigest", Value: bound}, {Key: "findings", Value: findings}}, nil
}
