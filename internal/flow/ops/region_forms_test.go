package ops

import (
	"slices"
	"testing"
)

// TestAdmissionRegionsDropsAnotherSpellingOfARegion (CRW-1404): an edit region of the DESIGN that is a region already admitted in
// another spelling (blanks around it, ./path, a doubled or a dotted separator) is that region, so it is not admitted twice; a region of
// another place, one that only overlaps it included, is admitted as before.
func TestAdmissionRegionsDropsAnotherSpellingOfARegion(t *testing.T) {
	for _, c := range []struct {
		name       string
		plan, edit []string
		want       []string
	}{
		{"whitespace", []string{"pkg/a.go", "docs/"}, []string{" pkg/a.go ", "\tdocs/ "}, []string{"pkg/a.go", "docs/"}},
		{"dot-slash", []string{"pkg/a.go", "docs/", "pkg/b.go#Run"}, []string{"./pkg/a.go", "./docs/", "./pkg/b.go#Run"}, []string{"pkg/a.go", "docs/", "pkg/b.go#Run"}},
		{"overlapping", []string{"pkg/a.go", "docs//"}, []string{"pkg//a.go", "pkg/./a.go", "docs/", "pkg/", "./pkg/", "pkg/a.go#Run"},
			[]string{"pkg/a.go", "docs//", "pkg/", "pkg/a.go#Run"}},
		{"repeated edit regions", []string{"pkg/a.go"}, []string{"x.go", " ./x.go", "x.go"}, []string{"pkg/a.go", "x.go"}},
		{"design only", nil, []string{"a.go", "./a.go", "b.go"}, []string{"a.go", "b.go"}},
		{"plan only", []string{"a.go"}, nil, []string{"a.go"}},
		{"both absent", nil, nil, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := AdmissionRegions(c.plan, c.edit); !slices.Equal(got, c.want) {
				t.Fatalf("AdmissionRegions(%q, %q) = %q, want %q", c.plan, c.edit, got, c.want)
			}
		})
	}
}
