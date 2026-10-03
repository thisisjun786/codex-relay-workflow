package review

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const testHead = "0123456789abcdef0123456789abcdef01234567"

// validArtifact is a partial run: three reviewers started, one timed out, two returned.
func validArtifact() Artifact {
	kept := finding(func(f *Finding) { f.Reviewers = []int{0, 1} })
	dropped := finding(func(f *Finding) { f.Grade, f.File = "", "x.go" })
	return Artifact{Schema: SchemaV1, Tool: "crw review", ToolVersion: "v0.4.0", Model: "claude-opus-5-5-high", Effort: "high", AgyVersion: "1.2.3",
		Base: strings.Repeat("a", 40), Head: testHead, PatchID: strings.Repeat("b", 40), Diff: DiffStats{Files: 3, Additions: 40, Deletions: 5},
		Reviewers: ReviewerCounts{Run: 3, Failed: 1, TimedOut: 1}, Status: StatusPartial, Reason: "reviewer 2 timed out",
		StartedAt: "2026-10-03T10:00:00Z", FinishedAt: "2026-10-03T10:07:30Z",
		Findings: []Finding{kept}, Dropped: []Drop{{Finding: dropped, Reason: ReasonNotInDiff, Detail: "x.go"}}}
}

func wantField(t *testing.T, err error, field string) {
	t.Helper()
	var ve ValidationError
	if !errors.As(err, &ve) || !slices.ContainsFunc(ve, func(fe FieldError) bool { return fe.Field == field }) {
		t.Errorf("want a refusal naming %q, got %v", field, err)
	}
}

func TestArtifactRoundTrip(t *testing.T) {
	want := validArtifact()
	data, err := want.Marshal(testHead)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseArtifact(data, testHead); err != nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("round trip failed: %v\n%+v", err, got)
	}
	// Apply -> Marshal -> ParseArtifact, with drops a strict check would refuse: an empty title and an empty grade.
	res, err := Rules{Changed: map[string]bool{"go.sum": true}, Head: fakeHead{"go.sum": 3}}.Apply([]Finding{
		finding(func(f *Finding) { f.Title, f.Explanation, f.Reviewers, f.Support = "", "", nil, 0 }),
		finding(func(f *Finding) { f.File, f.Grade = "go.sum", "" })})
	if err != nil || len(res.Dropped) != 2 {
		t.Fatalf("want two drops, got %+v, %v", res, err)
	}
	a := validArtifact()
	a.Findings, a.Dropped = res.Findings, res.Dropped
	if data, err = a.Marshal(testHead); err == nil {
		_, err = ParseArtifact(data, testHead)
	}
	if err != nil {
		t.Errorf("an artifact holding those drops must be accepted: %v", err)
	}
	// unavailable: nothing was started and the agy version is unknown
	un := Artifact{Schema: SchemaV1, Tool: "t", ToolVersion: "v", Model: "m", Effort: "e", AgyVersion: "unknown", Base: a.Base, Head: testHead,
		PatchID: a.PatchID, Status: StatusUnavailable, Reason: "agy login expired", StartedAt: a.StartedAt, FinishedAt: a.FinishedAt}
	if _, err := un.Marshal(testHead); err != nil {
		t.Errorf("an unavailable artifact must be accepted: %v", err)
	}
}

func TestValidateRefuses(t *testing.T) {
	for _, c := range []struct {
		name  string
		mut   func(*Artifact)
		field string
	}{
		{"schema", func(a *Artifact) { a.Schema = "v2" }, "schema"},
		{"blank model", func(a *Artifact) { a.Model = " " }, "model"},
		{"head mismatch", func(a *Artifact) { a.Head = strings.Repeat("c", 40) }, "head"},
		{"upper-case base", func(a *Artifact) { a.Base = strings.ToUpper(a.Base) }, "base"},
		{"failed above run", func(a *Artifact) { a.Reviewers.Failed = 4 }, "reviewers"},
		{"timed out above failed", func(a *Artifact) { a.Reviewers.TimedOut = 2 }, "reviewers"},
		{"status out of range", func(a *Artifact) { a.Status = "done" }, "status"},
		{"partial without a reason", func(a *Artifact) { a.Reason = "" }, "reason"},
		{"complete with a reason", func(a *Artifact) { a.Status, a.Reviewers = StatusComplete, ReviewerCounts{Run: 2} }, "reason"},
		{"complete with a failed reviewer", func(a *Artifact) { a.Status, a.Reason = StatusComplete, "" }, "status"},
		{"unavailable with findings", func(a *Artifact) { a.Status, a.Reviewers = StatusUnavailable, ReviewerCounts{Run: 1, Failed: 1} }, "status"},
		{"partial with nothing returned", func(a *Artifact) { a.Reviewers = ReviewerCounts{Run: 1, Failed: 1} }, "status"},
		{"bad start time", func(a *Artifact) { a.StartedAt = "yesterday" }, "startedAt"},
		{"finished before started", func(a *Artifact) { a.FinishedAt = "2026-10-03T09:00:00Z" }, "finishedAt"},
		{"grade out of range", func(a *Artifact) { a.Findings[0].Grade = "P4" }, "findings[0].grade"},
		{"rejected finding kept", func(a *Artifact) { a.Findings[0].Verdict = VerdictRejected }, "findings[0].verdict"},
		{"support differs from reviewers", func(a *Artifact) { a.Findings[0].Support = 3 }, "findings[0].support"},
		{"support above reviewers returned", func(a *Artifact) { a.Findings[0].Reviewers, a.Findings[0].Support = []int{0, 1, 2}, 3 }, "findings[0].support"},
		{"reviewers not ascending", func(a *Artifact) { a.Findings[0].Reviewers = []int{1, 0} }, "findings[0].reviewers"},
		{"absolute path", func(a *Artifact) { a.Findings[0].File = "/etc/x" }, "findings[0].file"},
		{"line zero", func(a *Artifact) { a.Findings[0].Line = 0 }, "findings[0].line"},
		{"blank title", func(a *Artifact) { a.Findings[0].Title = "" }, "findings[0].title"},
		{"dropped grade out of range", func(a *Artifact) { a.Dropped[0].Finding.Grade = "P9" }, "dropped[0].finding.grade"},
		{"unknown_grade with a grade", func(a *Artifact) { a.Dropped[0].Reason, a.Dropped[0].Finding.Grade = ReasonUnknownGrade, P1 }, "dropped[0].finding.grade"},
		{"unknown drop reason", func(a *Artifact) { a.Dropped[0].Reason = "because" }, "dropped[0].reason"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := validArtifact()
			c.mut(&a)
			wantField(t, a.Validate(testHead), c.field)
		})
	}
	a := validArtifact()
	wantField(t, a.Validate(""), "head")
}

func TestParseArtifactRefuses(t *testing.T) {
	first := func(m map[string]any) map[string]any { return m["findings"].([]any)[0].(map[string]any) }
	for _, c := range []struct {
		name  string
		mut   func(map[string]any)
		field string
	}{
		{"missing head", func(m map[string]any) { delete(m, "head") }, "head"},
		{"missing nested integer", func(m map[string]any) { delete(m["reviewers"].(map[string]any), "run") }, "reviewers.run"},
		{"missing finding field", func(m map[string]any) { delete(first(m), "grade") }, "findings[0].grade"},
		{"null findings", func(m map[string]any) { m["findings"] = nil }, "findings"},
		{"unknown field", func(m map[string]any) { m["extra"] = 1 }, "extra"},
		{"unknown nested field", func(m map[string]any) { first(m)["extra"] = 1 }, "findings[0].extra"},
		{"case-variant key", func(m map[string]any) { first(m)["GRADE"] = "P0" }, "findings[0].GRADE"},
		{"wrong type", func(m map[string]any) { first(m)["line"] = "3" }, "findings[0].line"},
		{"fractional integer", func(m map[string]any) { m["diff"].(map[string]any)["files"] = 1.5 }, "diff.files"},
	} {
		t.Run(c.name, func(t *testing.T) {
			data, _ := json.Marshal(validArtifact())
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			c.mut(m)
			data, _ = json.Marshal(m)
			_, err := ParseArtifact(data, testHead)
			wantField(t, err, c.field)
		})
	}
	good, _ := json.Marshal(validArtifact())
	for name, data := range map[string][]byte{"trailing data": append(good[:len(good):len(good)], " {}"...), "not an object": []byte("[]"), "null": []byte("null")} {
		if _, err := ParseArtifact(data, testHead); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func names[T ~string](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
}

// TestSchemaMatchesTypes keeps schema_v1.json in step with the Go types: the same properties, required keys, closed objects, enums,
// array and scalar types.
func TestSchemaMatchesTypes(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(SchemaV1JSON(), &root); err != nil {
		t.Fatal(err)
	}
	enums := map[reflect.Type][]string{reflect.TypeFor[Grade](): append([]string{""}, names(allGrades)...), reflect.TypeFor[Verdict](): names(allVerdicts),
		reflect.TypeFor[DropReason](): names(allReasons), reflect.TypeFor[Status](): names(allStatuses)}
	scalars := map[reflect.Kind]string{reflect.Int: "integer", reflect.String: "string", reflect.Bool: "boolean"}
	strs := func(v any) (out []string) {
		for _, s := range v.([]any) {
			out = append(out, s.(string))
		}
		return out
	}
	var check func(path string, ty reflect.Type, node map[string]any)
	check = func(path string, ty reflect.Type, node map[string]any) {
		if ref, ok := node["$ref"].(string); ok {
			node = root["definitions"].(map[string]any)[strings.TrimPrefix(ref, "#/definitions/")].(map[string]any)
		}
		switch want, isEnum := enums[ty]; {
		case isEnum:
			if got := strs(node["enum"]); !slices.Equal(got, want) {
				t.Errorf("%s: enum %v, want %v", path, got, want)
			}
		case node["const"] != nil:
			if node["const"] != SchemaV1 {
				t.Errorf("%s: const %v, want %s", path, node["const"], SchemaV1)
			}
		case ty.Kind() == reflect.Struct:
			props, _ := node["properties"].(map[string]any)
			var req []string
			for i := range ty.NumField() {
				name, opt, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
				if opt != "omitempty" {
					req = append(req, name)
				}
				if child, ok := props[name].(map[string]any); ok {
					check(path+"."+name, ty.Field(i).Type, child)
				} else {
					t.Errorf("%s: no schema property %q", path, name)
				}
			}
			got := strs(node["required"])
			slices.Sort(req)
			slices.Sort(got)
			if node["type"] != "object" || node["additionalProperties"] != false || len(props) != ty.NumField() || !slices.Equal(req, got) {
				t.Errorf("%s: want a closed object with %d properties and required %v, got required %v", path, ty.NumField(), req, got)
			}
		case ty.Kind() == reflect.Slice:
			if node["type"] != "array" {
				t.Fatalf("%s: must be an array", path)
			}
			check(path+"[]", ty.Elem(), node["items"].(map[string]any))
		case node["type"] != scalars[ty.Kind()]:
			t.Errorf("%s: type %v, want %s", path, node["type"], scalars[ty.Kind()])
		}
	}
	check("", reflect.TypeFor[Artifact](), root)
}
